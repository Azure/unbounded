//! The extent volume: slot table, index tree, superblocks, and I/O together.
//!
//! See `designs/racer-extent-volume.md`. A [`Volume`] is single threaded and
//! shared as `Rc<Volume<C>>`. Index operations are synchronous and in memory.
//! Data reads and writes, commits, and mount are [`Operation`]s driven by the
//! caller's reactor.
//!
//! # Write path
//!
//! 1. [`Volume::allocate`] (or [`Volume::allocate_evicting`]) reserves a record
//!    and returns a [`Reservation`].
//! 2. [`Volume::next_generation`] gives a unique write id. The caller puts it
//!    in the record header.
//! 3. [`Volume::write`] writes the record.
//! 4. [`Volume::publish`] inserts the index entry. It refuses a reservation
//!    whose write did not complete, so data always precedes its index entry.
//! 5. [`Volume::commit`] makes the entry durable. Storage writes must wait for
//!    it before they are acknowledged. Cache writes are committed lazily, for
//!    example by a timer that calls `commit` every second when
//!    [`Volume::is_dirty`].
//!
//! # Reuse of space
//!
//! Tree nodes and storage records that a durable superblock may still
//! reference are freed through the epoch rule of [`Slots`]: they are reused
//! only after two later commits succeed. Cache records, and storage slots
//! removed by eviction after their `retain_until` passed, are reused at once.
//! An older on-disk tree may therefore point at reused space for those. The
//! reader must check that the record header carries the entry's key and
//! generation, and treat a mismatch as a miss.

use std::any::Any;
use std::cell::{Cell, RefCell, RefMut};
use std::future::{Future, poll_fn};
use std::marker::PhantomData;
use std::ops::Bound;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Poll, Waker};

use uring_runtime::{Budget, Operation, Scope, reactor::Reactor};

use super::format::{
    Key, MAX_SLOTS, MIN_SLOTS, NodeAddr, SLOT_BYTES, SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT, Tier,
    UNIT_BYTES, Value, class_units, unit_offset,
};
use super::slots::{Allocation, Limits, SlotLease, SlotState, Slots, Usage};
use super::superblock::Superblock;
use super::tree::{CommitPlan, LoadReport, Loader, Tree};
use crate::{AlignedBuffer, Alignment, Charge, Error, Result, Slab};

/// Most node or superblock transfers a mount or commit keeps in flight.
pub const IO_CONCURRENCY: usize = 64;

/// Attempts per node read at mount before the copy counts as unreadable.
const READ_ATTEMPTS: usize = 3;

/// Generations skipped at mount, past any id handed out but not committed.
pub const GENERATION_SKIP: u64 = 1 << 40;

/// Cache slots a commit may evict to find space for tree nodes.
const COMMIT_EVICTIONS: usize = 16;

/// Builds the accounting guard for a metadata buffer of the given length.
pub type ChargeFn<C> = Rc<dyn Fn(usize) -> Result<C>>;

/// Options for [`Volume::mount`].
pub struct VolumeOptions<C: Charge> {
    /// Allocation limits. See [`Limits`].
    pub limits: Limits,

    /// Volume id written at format. Ignored when a volume is found. Should be
    /// random.
    pub volume_id: [u8; 16],

    /// Format a new, empty volume when no valid superblock is found. When
    /// false, mount fails with `Corrupt` instead.
    pub format: bool,

    /// Accounting guards for the volume's own 4096 byte node and superblock
    /// buffers.
    pub charge: ChargeFn<C>,
}

/// What [`Volume::mount`] found and repaired.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MountReport {
    /// No valid superblock was found and a new volume was formatted.
    pub formatted: bool,

    /// Seq of the mounted superblock.
    pub seq: u64,

    /// Unit of the mounted superblock in the meta slot (0 for A, 1 for B).
    pub superblock_unit: u16,

    /// Superblock units that were unreadable, invalid, of another geometry,
    /// or of another volume.
    pub invalid_superblocks: u8,

    /// The newest superblock's root was unreadable, so the older one was
    /// mounted.
    pub fell_back: bool,

    /// Key ranges `[low, high)` of the mounted tree whose node had no valid
    /// copy. `high` of `None` is unbounded. Entries in them were salvaged from
    /// the older tree where possible; others are gone.
    pub lost: Vec<(Key, Option<Key>)>,

    /// The mounted superblock's root had no valid copy. The tree started
    /// empty.
    pub root_lost: bool,

    /// Node copies that were unreadable or invalid in the mounted tree.
    pub invalid_copies: u64,

    /// Entries copied from the older tree into lost ranges.
    pub salvaged: u64,

    /// Entries removed because they were malformed or overlapped a node or an
    /// earlier entry.
    pub dropped: Vec<(Key, Value)>,

    /// Entries in the mounted index.
    pub entries: u64,

    /// Node units of the older tree reserved until the reuse rule frees them.
    pub pending_nodes: u64,

    /// Storage records of the older tree reserved until the reuse rule frees
    /// them.
    pub pending_records: u64,

    /// Whether the mounted index differs from the on-disk image (repairs,
    /// salvage, or drops). Commit to make the repaired index durable.
    pub dirty: bool,
}

/// Commit counters, for diagnostics and tests.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CommitStats {
    /// Commit attempts started.
    pub attempts: u64,

    /// Attempts that became durable.
    pub succeeded: u64,

    /// Attempts that failed or were canceled.
    pub failed: u64,

    /// Highest seq used, including failed attempts.
    pub seq: u64,

    /// Unit of the newest durable superblock (0 for A, 1 for B).
    pub durable_unit: u16,
}

/// Index state shared with reservations.
struct State {
    tree: Tree,
    slots: Slots,
    next_generation: u64,
    /// Bumped on every index change.
    mutations: u64,
}

impl State {
    /// Hand tree nodes superseded since the last call to the reuse rule.
    ///
    /// Each address is tagged with [`Slots::epoch`]: the first commit attempt
    /// that has not started yet. Every attempt that may reference the address
    /// started before it was taken from the tree, so its epoch is lower. The
    /// rule releases a tag only after a success with an epoch at least the tag
    /// (the first tree without the address) and one more success, so neither
    /// on-disk superblock references a released address.
    fn release_freed(&mut self) {
        for addr in self.tree.take_freed() {
            let epoch = self.slots.epoch();
            // Addresses never reserved in the table (for example skipped at
            // mount) report `Stale`; there is nothing to release.
            let _ = self.slots.free_node(addr, epoch);
        }
    }

    /// Remove the index entry of `key` and release its record.
    fn remove(&mut self, key: &Key) -> Option<Value> {
        let old = self.tree.remove(key)?;
        self.release(&old);
        self.mutations += 1;
        Some(old)
    }

    /// Release the record of a removed or replaced entry.
    fn release(&mut self, old: &Value) {
        let epoch = self.slots.epoch();
        let _ = self.slots.release_record(old.slot, old.unit, epoch);
    }

    /// Evict one data slot and remove its entries from the tree.
    fn evict(&mut self, now: u64) -> Option<Vec<(Key, Value)>> {
        let eviction = self.slots.evict_candidate(now)?;
        let mut removed = Vec::with_capacity(eviction.keys.len());
        for key in eviction.keys {
            if self
                .tree
                .get(&key)
                .is_some_and(|value| value.slot == eviction.slot)
            {
                let value = self.tree.remove(&key).expect("entry was just found");
                removed.push((key, value));
            }
        }
        self.slots
            .finish_eviction(eviction.slot)
            .expect("slot was chosen for eviction");
        if !removed.is_empty() {
            self.mutations += 1;
        }
        Some(removed)
    }
}

/// State reachable from reservations.
struct Shared {
    state: RefCell<State>,
    /// Reservations dropped while the state was borrowed.
    aborts: RefCell<Vec<Allocation>>,
}

impl Shared {
    /// Borrow the state, first aborting reservations dropped earlier.
    fn state(&self) -> RefMut<'_, State> {
        let mut state = self.state.borrow_mut();
        for allocation in self.aborts.borrow_mut().drain(..) {
            let _ = state.slots.abort(allocation);
        }
        state
    }
}

/// Wakeups for commit waiters, and the count of metadata writes in flight.
#[derive(Default)]
struct Signals {
    meta_writes: Cell<usize>,
    waiters: RefCell<Vec<Waker>>,
}

impl Signals {
    fn register(&self, waker: &Waker) {
        let mut waiters = self.waiters.borrow_mut();
        if !waiters.iter().any(|w| w.will_wake(waker)) {
            waiters.push(waker.clone());
        }
    }

    fn wake(&self) {
        let waiters = std::mem::take(&mut *self.waiters.borrow_mut());
        for waker in waiters {
            waker.wake();
        }
    }
}

/// Held by a node or superblock write until its completion.
struct MetaWrite(Rc<Signals>);

impl MetaWrite {
    fn new(signals: &Rc<Signals>) -> Self {
        signals.meta_writes.set(signals.meta_writes.get() + 1);
        Self(Rc::clone(signals))
    }
}

impl Drop for MetaWrite {
    fn drop(&mut self) {
        let count = self.0.meta_writes.get() - 1;
        self.0.meta_writes.set(count);
        if count == 0 {
            self.0.wake();
        }
    }
}

struct Failure {
    attempt: u64,
    covered: u64,
    error: Box<dyn Any>,
}

struct CommitState {
    running: bool,
    attempts: u64,
    finished: u64,
    succeeded: u64,
    failed: u64,
    durable_mutations: u64,
    failure: Option<Failure>,
    seq: u64,
    durable_unit: u16,
}

/// A reserved record. Dropping it without [`Volume::publish`] frees the
/// record, once no write to it is in flight.
pub struct Reservation {
    inner: Rc<Reserved>,
}

struct Reserved {
    shared: Rc<Shared>,
    allocation: RefCell<Option<Allocation>>,
    slot: u32,
    unit: u16,
    class: u8,
    tier: Tier,
    written: Cell<bool>,
}

impl Drop for Reserved {
    fn drop(&mut self) {
        if let Some(allocation) = self.allocation.get_mut().take() {
            match self.shared.state.try_borrow_mut() {
                Ok(mut state) => {
                    let _ = state.slots.abort(allocation);
                }
                Err(_) => self.shared.aborts.borrow_mut().push(allocation),
            }
        }
    }
}

impl Reservation {
    /// Data slot of the record.
    pub fn slot(&self) -> u32 {
        self.inner.slot
    }

    /// First unit of the record in its slot.
    pub fn unit(&self) -> u16 {
        self.inner.unit
    }

    /// Size class index.
    pub fn class(&self) -> u8 {
        self.inner.class
    }

    /// Tier.
    pub fn tier(&self) -> Tier {
        self.inner.tier
    }

    /// Byte offset of the record in the slab address space.
    pub fn offset(&self) -> u64 {
        unit_offset(self.inner.slot, self.inner.unit)
    }

    /// Most bytes the record can hold.
    pub fn capacity(&self) -> u64 {
        class_bytes(self.inner.class)
    }

    /// Whether a [`Volume::write`] to the record completed.
    pub fn is_written(&self) -> bool {
        self.inner.written.get()
    }
}

impl std::fmt::Debug for Reservation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Reservation")
            .field("slot", &self.inner.slot)
            .field("unit", &self.inner.unit)
            .field("class", &self.inner.class)
            .field("tier", &self.inner.tier)
            .field("written", &self.inner.written.get())
            .finish()
    }
}

fn class_bytes(class: u8) -> u64 {
    u64::from(class_units(class).unwrap_or(0)) * UNIT_BYTES as u64
}

fn other_unit(unit: u16) -> u16 {
    if unit == SUPERBLOCK_A_UNIT {
        SUPERBLOCK_B_UNIT
    } else {
        SUPERBLOCK_A_UNIT
    }
}

fn high_bound(high: Option<Key>) -> Bound<Key> {
    high.map_or(Bound::Unbounded, Bound::Excluded)
}

/// Run futures with at most `limit` in flight. Returns outputs in completion
/// order.
async fn bounded<'a, T: 'a>(
    mut futures: impl Iterator<Item = Pin<Box<dyn Future<Output = T> + 'a>>>,
    limit: usize,
) -> Vec<T> {
    let mut running: Vec<Pin<Box<dyn Future<Output = T> + 'a>>> = Vec::new();
    let mut outputs = Vec::new();
    let mut exhausted = false;
    poll_fn(|cx| {
        loop {
            while !exhausted && running.len() < limit {
                match futures.next() {
                    Some(future) => running.push(future),
                    None => exhausted = true,
                }
            }
            let mut progressed = false;
            let mut index = 0;
            while index < running.len() {
                if let Poll::Ready(output) = running[index].as_mut().poll(cx) {
                    outputs.push(output);
                    drop(running.swap_remove(index));
                    progressed = true;
                } else {
                    index += 1;
                }
            }
            if exhausted && running.is_empty() {
                return Poll::Ready(());
            }
            if !progressed {
                return Poll::Pending;
            }
        }
    })
    .await;
    outputs
}

/// Extent volume. See the module docs.
pub struct Volume<C: Charge> {
    slab: Rc<Slab<C>>,
    charge: ChargeFn<C>,
    alignment: Alignment,
    slot_count: u64,
    volume_id: [u8; 16],
    shared: Rc<Shared>,
    commit: RefCell<CommitState>,
    signals: Rc<Signals>,
}

/// A loaded tree and what loading it found.
struct Loaded {
    superblock: Superblock,
    unit: u16,
    tree: Tree,
    report: LoadReport,
}

impl<C: Charge> Volume<C> {
    /// Mount the volume on `slab`, or format it.
    ///
    /// The slab must use `SLOT_BYTES` segments and hold `MIN_SLOTS` to
    /// `MAX_SLOTS` of them; otherwise this fails with `InvalidConfiguration`.
    /// Its direct I/O alignment must divide 4096, or this fails with
    /// `Unsupported`. The slab is bound with [`Slab::open_volume`].
    ///
    /// Both superblocks are read. The newest valid one with this geometry and
    /// a slot count not above the slab's is mounted; if its root is
    /// unreadable, the other one is. Lost key ranges are salvaged from the
    /// other tree, the slot table is rebuilt, and space the other tree still
    /// references is reserved until two commits succeed. A superblock read
    /// error fails the mount rather than formatting over it. Node read errors
    /// are retried, then count as an invalid copy.
    ///
    /// Mount writes nothing unless it formats. A repaired index
    /// ([`MountReport::dirty`]) is durable only after a [`Volume::commit`].
    pub fn mount<'a, S: Scope, B: Budget>(
        reactor: &'a Reactor<S, B>,
        slab: Rc<Slab<C>>,
        options: VolumeOptions<C>,
        scope: &'a S,
    ) -> Operation<'a, (Rc<Self>, MountReport), S::Error>
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
            let VolumeOptions {
                limits,
                volume_id,
                format,
                charge,
            } = options;
            let mut volume = Self {
                slab,
                charge,
                alignment,
                slot_count,
                volume_id,
                shared: Rc::new(Shared {
                    state: RefCell::new(State {
                        tree: Tree::new(volume_id),
                        slots: Slots::new(slot_count, limits)?,
                        next_generation: 1,
                        mutations: 0,
                    }),
                    aborts: RefCell::new(Vec::new()),
                }),
                commit: RefCell::new(CommitState {
                    running: false,
                    attempts: 0,
                    finished: 0,
                    succeeded: 0,
                    failed: 0,
                    durable_mutations: 0,
                    failure: None,
                    seq: 0,
                    durable_unit: SUPERBLOCK_A_UNIT,
                }),
                signals: Rc::new(Signals::default()),
            };
            let report = volume.load(reactor, scope, limits, format).await?;
            Ok((Rc::new(volume), report))
        })
    }

    async fn read_unit<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        offset: u64,
        scope: &S,
    ) -> std::result::Result<Box<[u8; UNIT_BYTES]>, S::Error>
    where
        S::Error: From<Error>,
    {
        let buffer = self.slab.allocate(UNIT_BYTES, (self.charge)(UNIT_BYTES)?)?;
        let buffer = self
            .slab
            .read_at(reactor, offset, buffer, (), scope)
            .await?;
        let mut bytes = Box::new([0u8; UNIT_BYTES]);
        bytes.copy_from_slice(buffer.as_slice());
        Ok(bytes)
    }

    async fn read_node<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        addr: NodeAddr,
        scope: &S,
    ) -> Option<Box<[u8; UNIT_BYTES]>>
    where
        S::Error: From<Error>,
    {
        for _ in 0..READ_ATTEMPTS {
            if let Ok(bytes) = self.read_unit(reactor, addr.offset(), scope).await {
                return Some(bytes);
            }
        }
        None
    }

    async fn load_tree<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        superblock: &Superblock,
    ) -> std::result::Result<(Tree, LoadReport), S::Error>
    where
        S::Error: From<Error>,
    {
        let mut loader = Loader::new(
            superblock.volume_id,
            self.slot_count,
            superblock.seq,
            superblock.root,
            superblock.root_level,
            superblock.entries,
        );
        loop {
            let reads = loader.next_reads();
            if reads.is_empty() {
                break;
            }
            scope.check()?;
            let results: Vec<(NodeAddr, Option<Box<[u8; UNIT_BYTES]>>)> = bounded(
                reads.into_iter().map(|addr| {
                    Box::pin(async move { (addr, self.read_node(reactor, addr, scope).await) })
                        as Pin<Box<dyn Future<Output = _> + '_>>
                }),
                IO_CONCURRENCY,
            )
            .await;
            for (addr, bytes) in results {
                loader.supply(addr, bytes.as_deref());
            }
        }
        Ok(loader.finish())
    }

    async fn load<S: Scope, B: Budget>(
        &mut self,
        reactor: &Reactor<S, B>,
        scope: &S,
        limits: Limits,
        format: bool,
    ) -> std::result::Result<MountReport, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut report = MountReport::default();
        let mut candidates = Vec::new();
        for unit in [SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT] {
            let mut last = None;
            let mut bytes = None;
            for _ in 0..READ_ATTEMPTS {
                match self.read_unit(reactor, unit_offset(0, unit), scope).await {
                    Ok(b) => {
                        bytes = Some(b);
                        break;
                    }
                    Err(error) => last = Some(error),
                }
            }
            let Some(bytes) = bytes else {
                return Err(last.expect("a read failed"));
            };
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
            if !format {
                return Err(Error::Corrupt.into());
            }
            self.format(reactor, scope).await?;
            report.formatted = true;
            report.seq = 1;
            report.superblock_unit = SUPERBLOCK_A_UNIT;
            return Ok(report);
        }
        let highest_seq = candidates[0].0.seq;
        self.volume_id = candidates[0].0.volume_id;

        let mut loads = Vec::new();
        for (superblock, unit) in &candidates {
            let (tree, load_report) = self.load_tree(reactor, scope, superblock).await?;
            loads.push(Loaded {
                superblock: *superblock,
                unit: *unit,
                tree,
                report: load_report,
            });
        }
        let mut mounted = loads.remove(0);
        let mut other = loads.pop();
        if mounted.report.root_lost && other.as_ref().is_some_and(|other| !other.report.root_lost) {
            std::mem::swap(&mut mounted, other.as_mut().expect("checked"));
            report.fell_back = true;
        }
        // The other tree is only useful when it differs.
        if other
            .as_ref()
            .is_some_and(|o| o.superblock.seq == mounted.superblock.seq)
        {
            other = None;
        }

        let Loaded {
            superblock,
            unit,
            mut tree,
            report: load,
        } = mounted;
        report.seq = superblock.seq;
        report.superblock_unit = unit;
        report.lost = load.lost.clone();
        report.root_lost = load.root_lost;
        report.invalid_copies = load.invalid_copies;

        if let Some(other) = &other {
            for (low, high) in &load.lost {
                for (key, value) in other.tree.range(Bound::Included(*low), high_bound(*high)) {
                    if tree.get(&key).is_none() {
                        tree.insert(key, value);
                        report.salvaged += 1;
                    }
                }
            }
        }

        // Addresses the mounted superblock references but whose node is
        // rewritten (repaired copies) or unreadable. They stay reserved until
        // the reuse rule frees them.
        let mut referenced = tree.take_freed();
        referenced.extend(load.lost_addrs.iter().copied());
        let nodes: Vec<NodeAddr> = tree
            .node_addrs()
            .chain(referenced.iter().copied())
            .collect();
        let entries: Vec<(Key, Value)> = tree.range(Bound::Unbounded, Bound::Unbounded).collect();
        let (mut slots, dropped) = Slots::rebuild(self.slot_count, limits, nodes, entries)?;
        for (key, value) in &dropped {
            if tree.get(key) == Some(*value) {
                tree.remove(key);
            }
        }
        report.dropped = dropped;
        let epoch = slots.epoch();
        for addr in referenced {
            let _ = slots.free_node(addr, epoch);
        }
        // Freed by the removals above; nothing on disk references them yet,
        // but tagging them is harmless.
        for addr in tree.take_freed() {
            let _ = slots.free_node(addr, epoch);
        }
        let mut max_generation = tree
            .range(Bound::Unbounded, Bound::Unbounded)
            .map(|(_, v)| v.generation)
            .max()
            .unwrap_or(0);
        if let Some(mut other) = other {
            let mut addrs: Vec<NodeAddr> = other.tree.node_addrs().collect();
            addrs.extend(other.tree.take_freed());
            addrs.extend(other.report.lost_addrs.iter().copied());
            report.pending_nodes = slots.mark_pending_nodes(addrs, epoch) as u64;
            let values: Vec<Value> = other
                .tree
                .range(Bound::Unbounded, Bound::Unbounded)
                .map(|(_, v)| v)
                .collect();
            max_generation = values
                .iter()
                .map(|v| v.generation)
                .fold(max_generation, u64::max);
            report.pending_records = slots.mark_pending_records(values, epoch) as u64;
        }
        report.entries = tree.len();
        report.dirty = tree.is_dirty();
        let next_generation = superblock
            .next_generation
            .max(max_generation.saturating_add(1))
            .saturating_add(GENERATION_SKIP);

        let mut state = self.shared.state.borrow_mut();
        state.tree = tree;
        state.slots = slots;
        state.next_generation = next_generation;
        state.mutations = u64::from(report.dirty);
        let commit = self.commit.get_mut();
        commit.seq = highest_seq;
        commit.durable_unit = unit;
        Ok(report)
    }

    async fn format<S: Scope, B: Budget>(
        &mut self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let superblock = Superblock::empty(self.volume_id, 1, self.slot_count, 1);
        self.write_unit(
            reactor,
            unit_offset(0, SUPERBLOCK_A_UNIT),
            &superblock.encode(),
            scope,
        )
        .await?;
        self.slab.sync(reactor, scope).await?;
        let commit = self.commit.get_mut();
        commit.seq = 1;
        commit.durable_unit = SUPERBLOCK_A_UNIT;
        Ok(())
    }

    async fn write_unit<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        offset: u64,
        bytes: &[u8; UNIT_BYTES],
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let mut buffer = self.slab.allocate(UNIT_BYTES, (self.charge)(UNIT_BYTES)?)?;
        buffer.as_mut_slice().copy_from_slice(bytes);
        let guard = MetaWrite::new(&self.signals);
        let _ = self
            .slab
            .write_at(reactor, offset, buffer, guard, scope)
            .await?;
        Ok(())
    }

    /// Slots in the volume, including the meta slot.
    pub fn slot_count(&self) -> u64 {
        self.slot_count
    }

    /// Direct I/O alignment of the slab. Data buffers must follow it.
    pub fn alignment(&self) -> Alignment {
        self.alignment
    }

    /// Volume id.
    pub fn volume_id(&self) -> [u8; 16] {
        self.volume_id
    }

    /// Entries in the index.
    pub fn len(&self) -> u64 {
        self.shared.state().tree.len()
    }

    /// Whether the index has no entries.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Whether the index has changes that are not durable yet.
    pub fn is_dirty(&self) -> bool {
        let state = self.shared.state();
        state.tree.is_dirty() || state.mutations > self.commit.borrow().durable_mutations
    }

    /// Value of `key`.
    pub fn get(&self, key: &Key) -> Option<Value> {
        self.shared.state().tree.get(key)
    }

    /// Entries with keys in `(low, high)`, in key order, at most `limit`.
    pub fn range(&self, low: Bound<Key>, high: Bound<Key>, limit: usize) -> Vec<(Key, Value)> {
        self.shared
            .state()
            .tree
            .range(low, high)
            .take(limit)
            .collect()
    }

    /// Up to `limit` entries with keys after `after` (from the start if
    /// `None`), in key order.
    pub fn next_after(&self, after: Option<&Key>, limit: usize) -> Vec<(Key, Value)> {
        self.shared.state().tree.next_after(after, limit)
    }

    /// Pin a slot so eviction skips it. `None` when out of range.
    pub fn lease(&self, slot: u32) -> Option<SlotLease> {
        (u64::from(slot) < self.slot_count).then(|| self.shared.state().slots.lease(slot))
    }

    /// Read a record into `buffer`, from `value.offset()` for `buffer.len()`
    /// bytes. The buffer must follow [`Volume::alignment`] and fit the
    /// record's class, or this fails with `InvalidConfiguration`.
    ///
    /// The slot is leased when this is called, held until the read completes,
    /// and its CLOCK reference bit is set. The slot may have been evicted and
    /// reused between the lookup and this call, so the caller must check the
    /// record header (key and generation) before using the bytes.
    pub fn read<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        value: &Value,
        buffer: AlignedBuffer<C>,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        let value = *value;
        let checked = if !value.is_well_formed()
            || u64::from(value.slot) >= self.slot_count
            || buffer.len() as u64 > class_bytes(value.class)
        {
            Err(Error::InvalidConfiguration)
        } else {
            let mut state = self.shared.state();
            state.slots.touch(value.slot);
            Ok(state.slots.lease(value.slot))
        };
        Box::pin(async move {
            let lease = checked?;
            self.slab
                .read_at(reactor, value.offset(), buffer, lease, scope)
                .await
        })
    }

    /// Reserve a record of `length` bytes in `tier`. The slot is leased until
    /// the reservation is published or dropped.
    ///
    /// Fails with `InvalidConfiguration` when `length` is 0 or above
    /// `SLOT_BYTES`, and with `Busy` when there is no free slot, the storage
    /// quota is reached, or the small-record byte cap would be exceeded. On
    /// `Busy`, evict ([`Volume::evict_one`]) and retry, or use
    /// [`Volume::allocate_evicting`].
    pub fn allocate(&self, tier: Tier, length: u64) -> Result<Reservation> {
        let allocation = self.shared.state().slots.allocate(tier, length)?;
        Ok(Reservation {
            inner: Rc::new(Reserved {
                shared: Rc::clone(&self.shared),
                slot: allocation.slot,
                unit: allocation.unit,
                class: allocation.class,
                tier,
                written: Cell::new(false),
                allocation: RefCell::new(Some(allocation)),
            }),
        })
    }

    /// [`Volume::allocate`], evicting up to `max_evictions` slots while it
    /// returns `Busy`. Returns the reservation and the entries removed by
    /// eviction (also when allocation still fails, they are gone).
    pub fn allocate_evicting(
        &self,
        tier: Tier,
        length: u64,
        now: u64,
        max_evictions: usize,
    ) -> (Result<Reservation>, Vec<(Key, Value)>) {
        let mut evicted = Vec::new();
        let mut evictions = 0;
        loop {
            match self.allocate(tier, length) {
                Err(Error::Busy) if evictions < max_evictions => {
                    evictions += 1;
                    match self.evict_one(now) {
                        Some(removed) => evicted.extend(removed),
                        None => return (Err(Error::Busy), evicted),
                    }
                }
                result => return (result, evicted),
            }
        }
    }

    /// Evict one data slot with the CLOCK policy and remove its entries from
    /// the index. Returns the removed entries, or `None` when no slot can be
    /// evicted.
    ///
    /// A slot can be evicted when it is not open, not leased, its reference
    /// bit is clear, and it is a cache slot or a storage slot whose largest
    /// `retain_until` is below `now`. The slot is free at once, even for
    /// storage: an older on-disk tree that still points into it is caught by
    /// the record header check.
    pub fn evict_one(&self, now: u64) -> Option<Vec<(Key, Value)>> {
        self.shared.state().evict(now)
    }

    /// A unique, nonzero write id, larger than any id handed out or committed
    /// before on this volume (mount skips `GENERATION_SKIP` ids past the
    /// committed high-water mark).
    pub fn next_generation(&self) -> u64 {
        let mut state = self.shared.state();
        let generation = state.next_generation;
        state.next_generation += 1;
        generation
    }

    /// Write `buffer` at the start of the reserved record. The buffer must
    /// follow [`Volume::alignment`] and fit the record's class, or this fails
    /// with `InvalidConfiguration`. The reservation stays reserved until the
    /// write completes, even if this future is dropped.
    pub fn write<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        reservation: &Reservation,
        buffer: AlignedBuffer<C>,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        let inner = Rc::clone(&reservation.inner);
        let checked = if buffer.len() as u64 > class_bytes(inner.class)
            || inner.allocation.borrow().is_none()
        {
            Err(Error::InvalidConfiguration)
        } else {
            Ok(())
        };
        Box::pin(async move {
            checked?;
            let offset = unit_offset(inner.slot, inner.unit);
            let buffer = self
                .slab
                .write_at(reactor, offset, buffer, Rc::clone(&inner), scope)
                .await?;
            inner.written.set(true);
            Ok(buffer)
        })
    }

    /// Insert `key` for the written reservation. Returns the previous value of
    /// `key`, whose record is released: at once for cache, by the reuse rule
    /// for storage.
    ///
    /// `generation` must be the id in the record header (normally from
    /// [`Volume::next_generation`]) and `length` the bytes written. Fails with
    /// `InvalidConfiguration` when the reservation was not written, `length`
    /// is 0 or above its capacity, or `generation` is 0, and with `Busy` while
    /// a write to it is in flight. On error the reservation is dropped, which
    /// frees it.
    pub fn publish(
        &self,
        key: Key,
        reservation: Reservation,
        length: u32,
        generation: u64,
        retain_until: u64,
    ) -> Result<Option<Value>> {
        let inner = reservation.inner;
        if Rc::strong_count(&inner) != 1 {
            return Err(Error::Busy);
        }
        if !inner.written.get()
            || length == 0
            || u64::from(length) > class_bytes(inner.class)
            || generation == 0
        {
            return Err(Error::InvalidConfiguration);
        }
        let allocation = inner.allocation.borrow_mut().take().ok_or(Error::Stale)?;
        let value = Value {
            slot: inner.slot,
            unit: inner.unit,
            class: inner.class,
            tier: inner.tier,
            length,
            flags: 0,
            generation,
            retain_until,
        };
        drop(inner);
        let mut state = self.shared.state();
        let old = state.tree.insert(key, value);
        if let Err(error) = state
            .slots
            .commit_record(value.slot, value.unit, key, retain_until)
        {
            match old {
                Some(old) => state.tree.insert(key, old),
                None => state.tree.remove(&key),
            };
            let _ = state.slots.abort(allocation);
            return Err(error);
        }
        if let Some(old) = &old {
            state.release(old);
        }
        state.mutations += 1;
        drop(allocation);
        Ok(old)
    }

    /// Remove `key`. Returns its value. The record is released: at once for
    /// cache, by the reuse rule for storage.
    pub fn remove(&self, key: &Key) -> Option<Value> {
        self.shared.state().remove(key)
    }

    /// Remove `key` only if its value is `expected`. Returns whether it was
    /// removed.
    pub fn remove_if(&self, key: &Key, expected: &Value) -> bool {
        let mut state = self.shared.state();
        if state.tree.get(key) != Some(*expected) {
            return false;
        }
        state.remove(key).is_some()
    }

    /// Remove every entry with a key in `(low, high)`. Returns the removed
    /// entries in key order.
    pub fn remove_range(&self, low: Bound<Key>, high: Bound<Key>) -> Vec<(Key, Value)> {
        let mut state = self.shared.state();
        let entries: Vec<(Key, Value)> = state.tree.range(low, high).collect();
        for (key, _) in &entries {
            state.remove(key);
        }
        entries
    }

    /// Set the `retain_until` of `key`'s entry and update its slot's
    /// protection. Returns whether `key` was found.
    pub fn set_retain(&self, key: &Key, until: u64) -> bool {
        let mut state = self.shared.state();
        let Some(old) = state.tree.get(key) else {
            return false;
        };
        if old.retain_until == until {
            return true;
        }
        state.tree.update(key, |value| value.retain_until = until);
        state.mutations += 1;
        if until > old.retain_until {
            state.slots.retain(old.slot, until);
        } else {
            let slot = old.slot;
            let keys = state.slots.keys(slot);
            let max = keys
                .iter()
                .filter_map(|k| state.tree.get(k))
                .filter(|v| v.slot == slot)
                .map(|v| v.retain_until)
                .max()
                .unwrap_or(0);
            state.slots.set_retain(slot, max);
        }
        true
    }

    /// Occupancy summary.
    pub fn usage(&self) -> Usage {
        self.shared.state().slots.usage()
    }

    /// State of one slot, or `None` when out of range. Slot `i` covers bytes
    /// `[i * SLOT_BYTES, (i + 1) * SLOT_BYTES)` of the slab, which lets the
    /// caller attribute usage to backing devices.
    pub fn slot_state(&self, slot: u32) -> Option<SlotState> {
        self.shared.state().slots.slot_state(slot)
    }

    /// Current allocation limits.
    pub fn limits(&self) -> Limits {
        self.shared.state().slots.limits()
    }

    /// Replace the allocation limits. Space already in use is kept.
    pub fn set_limits(&self, limits: Limits) {
        self.shared.state().slots.set_limits(limits);
    }

    /// Commit counters.
    pub fn commit_stats(&self) -> CommitStats {
        let commit = self.commit.borrow();
        CommitStats {
            attempts: commit.attempts,
            succeeded: commit.succeeded,
            failed: commit.failed,
            seq: commit.seq,
            durable_unit: commit.durable_unit,
        }
    }

    /// Make every index change made before this call durable.
    ///
    /// Concurrent callers share one commit attempt. A caller whose changes
    /// came after the running attempt started waits for it and then runs (or
    /// joins) a follow-up attempt. A failed attempt reports its error to every
    /// caller it covered; the volume stays usable and the next commit retries
    /// the same changes. Dropping the future of the caller running an attempt
    /// fails that attempt with `Cancelled` for the other callers.
    ///
    /// An attempt writes every dirty node to new addresses, fsyncs (which
    /// also makes all published data durable, because entries are published
    /// only after their write completed), writes the superblock that does not
    /// hold the newest durable one, and fsyncs again. When there is no space
    /// for nodes, it evicts cache slots.
    pub fn commit<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        let target = self.shared.state().mutations;
        let finished_at_call = self.commit.borrow().finished;
        Box::pin(async move {
            loop {
                let wait_for = {
                    let commit = self.commit.borrow();
                    if commit.durable_mutations >= target {
                        return Ok(());
                    }
                    if let Some(failure) = &commit.failure
                        && failure.attempt > finished_at_call
                        && failure.covered >= target
                    {
                        return Err(failure
                            .error
                            .downcast_ref::<S::Error>()
                            .copied()
                            .unwrap_or_else(|| uring_runtime::Error::Io.into()));
                    }
                    commit.running.then_some(commit.attempts)
                };
                match wait_for {
                    Some(attempt) => {
                        poll_fn(|cx| {
                            if self.commit.borrow().finished >= attempt {
                                Poll::Ready(())
                            } else {
                                self.signals.register(cx.waker());
                                Poll::Pending
                            }
                        })
                        .await
                    }
                    None => return self.attempt(reactor, scope).await,
                }
            }
        })
    }

    async fn attempt<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let id = {
            let mut commit = self.commit.borrow_mut();
            commit.running = true;
            commit.attempts += 1;
            commit.attempts
        };
        let mut attempt = Attempt::<C, S::Error> {
            volume: self,
            id,
            plan: None,
            epoch: None,
            covered: 0,
            done: false,
            error: PhantomData,
        };
        // A canceled attempt may leave writes in flight. Wait for them so they
        // cannot land after this attempt reuses an address or superblock.
        poll_fn(|cx| {
            if self.signals.meta_writes.get() == 0 {
                Poll::Ready(())
            } else {
                self.signals.register(cx.waker());
                Poll::Pending
            }
        })
        .await;
        if let Err(error) = scope.check() {
            return attempt.fail(error);
        }
        let (mut plan, next_generation) = {
            let mut state = self.shared.state();
            state.release_freed();
            let epoch = state.slots.begin_commit();
            attempt.epoch = Some(epoch);
            let seq = {
                let mut commit = self.commit.borrow_mut();
                commit.seq += 1;
                commit.seq
            };
            let mut evictions = 0;
            let plan = loop {
                let State { tree, slots, .. } = &mut *state;
                match tree.prepare_commit(seq, &mut || slots.alloc_node()) {
                    Ok(plan) => break plan,
                    Err(Error::Busy) if evictions < COMMIT_EVICTIONS => {
                        evictions += 1;
                        state.release_freed();
                        // `now = 0` never evicts protected storage.
                        if state.evict(0).is_none() {
                            drop(state);
                            return attempt.fail(Error::Busy.into());
                        }
                    }
                    Err(error) => {
                        drop(state);
                        return attempt.fail(error.into());
                    }
                }
            };
            attempt.covered = state.mutations;
            (plan, state.next_generation)
        };
        let writes = std::mem::take(&mut plan.writes);
        let superblock = Superblock {
            volume_id: self.volume_id,
            seq: plan.seq,
            unit_bytes: UNIT_BYTES as u32,
            slot_units: super::format::SLOT_UNITS,
            slot_count: self.slot_count,
            root: plan.root,
            root_level: plan.root_level,
            entries: plan.entries,
            next_generation,
        };
        attempt.plan = Some(plan);
        let results: Vec<std::result::Result<(), S::Error>> = bounded(
            writes.iter().map(|(addr, bytes)| {
                Box::pin(self.write_unit(reactor, addr.offset(), bytes, scope))
                    as Pin<Box<dyn Future<Output = _> + '_>>
            }),
            IO_CONCURRENCY,
        )
        .await;
        if let Some(Err(error)) = results.into_iter().find(|r| r.is_err()) {
            return attempt.fail(error);
        }
        if let Err(error) = self.slab.sync(reactor, scope).await {
            return attempt.fail(error);
        }
        let unit = other_unit(self.commit.borrow().durable_unit);
        if let Err(error) = self
            .write_unit(reactor, unit_offset(0, unit), &superblock.encode(), scope)
            .await
        {
            return attempt.fail(error);
        }
        if let Err(error) = self.slab.sync(reactor, scope).await {
            return attempt.fail(error);
        }
        attempt.succeed(unit);
        Ok(())
    }
}

/// One running commit attempt. Dropping it unfinished fails it.
struct Attempt<'v, C: Charge, E: Copy + 'static + From<uring_runtime::Error>> {
    volume: &'v Volume<C>,
    id: u64,
    plan: Option<CommitPlan>,
    epoch: Option<u64>,
    covered: u64,
    done: bool,
    error: PhantomData<E>,
}

impl<C: Charge, E: Copy + 'static + From<uring_runtime::Error>> Attempt<'_, C, E> {
    fn succeed(&mut self, unit: u16) {
        self.finish(None, Some(unit));
    }

    fn fail(&mut self, error: E) -> std::result::Result<(), E> {
        self.finish(Some(error), None);
        Err(error)
    }

    fn finish(&mut self, error: Option<E>, unit: Option<u16>) {
        self.done = true;
        let volume = self.volume;
        {
            let mut state = volume.shared.state();
            let State { tree, slots, .. } = &mut *state;
            match (&error, self.plan.take()) {
                (None, Some(plan)) => tree.commit_succeeded(plan),
                (_, Some(plan)) => tree.commit_failed(plan),
                (_, None) => {}
            }
            if let Some(epoch) = self.epoch {
                if error.is_none() {
                    slots.commit_succeeded(epoch);
                } else {
                    slots.commit_failed(epoch);
                }
            }
            state.release_freed();
        }
        let mut commit = volume.commit.borrow_mut();
        commit.running = false;
        commit.finished = self.id;
        match error {
            None => {
                commit.succeeded += 1;
                commit.durable_mutations = commit.durable_mutations.max(self.covered);
                if let Some(unit) = unit {
                    commit.durable_unit = unit;
                }
            }
            Some(error) => {
                commit.failed += 1;
                commit.failure = Some(Failure {
                    attempt: self.id,
                    covered: self.covered,
                    error: Box::new(error),
                });
            }
        }
        drop(commit);
        volume.signals.wake();
    }
}

impl<C: Charge, E: Copy + 'static + From<uring_runtime::Error>> Drop for Attempt<'_, C, E> {
    fn drop(&mut self) {
        if !self.done {
            self.finish(Some(uring_runtime::Error::Cancelled.into()), None);
        }
    }
}
