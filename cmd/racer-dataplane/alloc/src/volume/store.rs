//! Extent volume: slot table, paged trees, overlays, and I/O together.
//!
//! See `designs/racer-extent-volume.md`. A [`Volume`] is single threaded and
//! shared as `Rc<Volume<C>>`. Index state lives on disk in copy-on-write
//! trees; only a bounded cache of their pages ([`VolumeOptions::metadata_cache_bytes`])
//! and the changes since the last commit (the overlay) are in memory.
//! Lookups that miss the cache read pages from disk, so they are
//! [`Operation`]s; [`Volume::try_get`] answers from memory only.
//!
//! # Write path
//!
//! 1. [`Volume::allocate`] (or [`Volume::allocate_evicting`]) reserves a record
//!    and returns a [`Reservation`].
//! 2. [`Volume::next_generation`] gives a unique write id. The caller puts it
//!    in the record header.
//! 3. [`Volume::write`] writes the record.
//! 4. [`Volume::publish`] records the index entry in the overlay. It refuses
//!    a reservation whose write did not complete, so data always precedes
//!    its index entry.
//! 5. [`Volume::commit`] makes the entry durable. Storage writes must wait for
//!    it before they are acknowledged. Cache writes are committed lazily, for
//!    example by a timer that calls `commit` when [`Volume::is_dirty`].
//!
//! # Reuse of space
//!
//! Metadata pages and storage records that a durable superblock may still
//! reference are listed in the retired log of the commit that stopped
//! referencing them, and become reusable only after the next commit is
//! durable. Cache records, and slots emptied by eviction, are reused at once.
//! An older on-disk tree may therefore point at reused space for those. The
//! reader must check that the record header carries the entry's key and
//! generation, and treat a mismatch as a miss.

use std::any::Any;
use std::cell::{Cell, RefCell, RefMut};
use std::collections::BTreeMap;
use std::future::{Future, poll_fn};
use std::ops::Bound;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Poll, Waker};

use uring_runtime::{Budget, Operation, Scope, reactor::Reactor};

use super::btree::{self, LocalFuture, Lookup, Req, Seek, TreeRoot};
use super::format::{Key, NodeAddr, SLOT_BYTES, Tier, UNIT_BYTES, Value, class_units, unit_offset};
use super::page::{RecordDesc, Retired, TreeKind, TreePage, record_key, record_key_parts};
use super::pager::{FaultResult, FaultRole, Pager, PagerStats, Priority};
use super::table::{Allocation, Limits, SlotLease, SlotState, Slots, Usage};
use crate::{AlignedBuffer, Alignment, Charge, Error, Result, Slab};

/// Most metadata transfers a mount or commit keeps in flight.
pub const IO_CONCURRENCY: usize = 64;

/// Attempts per page read before the copy counts as unreadable.
pub(super) const READ_ATTEMPTS: usize = 3;

/// Generations skipped at mount, past any id handed out but not committed.
pub const GENERATION_SKIP: u64 = 1 << 40;

/// Default byte budget of the metadata page cache.
pub const DEFAULT_METADATA_CACHE_BYTES: usize = 64 << 20;

/// Smallest default dirty limit (about 50k uncommitted changes).
pub const DEFAULT_DIRTY_BYTES: usize = 8 << 20;

/// Records an eviction step handles before yielding to other tasks.
pub const EVICT_BATCH: usize = 1024;

/// Bytes charged per overlay entry.
pub(super) const OVERLAY_ENTRY_BYTES: usize = 160;

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

    /// Accounting guards for the volume's own metadata I/O buffers.
    pub charge: ChargeFn<C>,

    /// Byte budget of the metadata page cache. Pages in use may exceed it
    /// briefly.
    pub metadata_cache_bytes: usize,

    /// Bytes of uncommitted index changes above which allocation returns
    /// `Busy` until a commit. 0 picks a quarter of the cache budget, at
    /// least [`DEFAULT_DIRTY_BYTES`].
    pub dirty_bytes: usize,
}

impl<C: Charge> VolumeOptions<C> {
    /// Options with the default cache budget.
    pub fn new(limits: Limits, volume_id: [u8; 16], format: bool, charge: ChargeFn<C>) -> Self {
        Self {
            limits,
            volume_id,
            format,
            charge,
            metadata_cache_bytes: DEFAULT_METADATA_CACHE_BYTES,
            dirty_bytes: 0,
        }
    }
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

    /// The newest superblock's allocation metadata (slot table or retired
    /// log) was unreadable, so the older one was mounted.
    pub fell_back: bool,

    /// No root set had readable allocation metadata; the newest was mounted
    /// with the unreadable parts quarantined.
    pub root_lost: bool,

    /// Slot table key ranges `[low, high)` that were unreadable; their slots
    /// are quarantined.
    pub lost: Vec<(Key, Option<Key>)>,

    /// Metadata page copies that were invalid while one copy was valid.
    pub invalid_copies: u64,

    /// Node slots whose bitmap was rebuilt by walking the trees.
    pub rebuilt_bitmaps: u64,

    /// Entries in the mounted index.
    pub entries: u64,

    /// Metadata pages reserved until the next commit releases them.
    pub pending_nodes: u64,

    /// Storage records reserved until the next commit releases them.
    pub pending_records: u64,

    /// Metadata pages read by the mount.
    pub pages_read: u64,

    /// Whether the mount repaired allocation metadata (rebuilt bitmaps or
    /// quarantined slots) that only a commit makes durable. The volume is
    /// dirty after any mount that did not format regardless: the first
    /// commit persists the generation skip.
    pub dirty: bool,
}

/// What [`Volume::scrub`] found and repaired.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ScrubReport {
    /// Index key ranges `[low, high)` whose page had no valid copy. Their
    /// entries are gone; the next commit that touches a range rewrites it.
    pub lost: Vec<(Key, Option<Key>)>,

    /// Record map ranges whose page had no valid copy.
    pub lost_records: Vec<(Key, Option<Key>)>,

    /// Index entries removed because they were malformed or did not lie in
    /// a data slot of their tier and class.
    pub dropped: Vec<(Key, Value)>,

    /// Index entries visited.
    pub entries: u64,

    /// Pages read from disk by the scrub.
    pub pages_read: u64,

    /// Metadata page copies that were invalid while one copy was valid.
    pub invalid_copies: u64,
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

    /// Metadata pages written by the last successful attempt.
    pub pages_written: u64,

    /// Metadata write requests submitted by the last successful attempt.
    pub writes: u64,
}

/// Uncommitted change to an index entry.
#[derive(Clone, Copy, Debug)]
pub(super) enum IndexOp {
    Put(Value),
    Delete,
}

#[derive(Clone, Copy, Debug)]
pub(super) struct IndexEntry {
    pub(super) op: IndexOp,
}

/// Uncommitted change to a record map entry.
#[derive(Clone, Copy, Debug)]
pub(super) enum RecOp {
    /// A published record.
    Put(RecordDesc),

    /// The index stopped referencing this record: free it (cache) or retire
    /// it (storage) if the entry still names this key and generation.
    Release { key: Key, generation: u64 },

    /// A retired storage record of this incarnation is reusable.
    Retired { incarnation: u32 },
}

/// One layer of uncommitted changes.
#[derive(Clone, Debug, Default)]
pub(super) struct Overlay {
    pub(super) index: BTreeMap<Key, IndexEntry>,
    pub(super) records: BTreeMap<Key, RecOp>,
    /// Change of the index entry count from this layer.
    pub(super) delta: i64,
}

impl Overlay {
    pub(super) fn is_empty(&self) -> bool {
        self.index.is_empty() && self.records.is_empty()
    }

    pub(super) fn entries(&self) -> usize {
        self.index.len() + self.records.len()
    }

    /// Fold a later layer into this one. Records published and released
    /// within the two layers never reach disk; they are freed in `slots` at
    /// once.
    pub(super) fn absorb(&mut self, later: Overlay, slots: &mut Slots) {
        for (key, entry) in later.index {
            match self.index.get_mut(&key) {
                Some(earlier) => earlier.op = entry.op,
                None => {
                    self.index.insert(key, entry);
                }
            }
        }
        for (rk, op) in later.records {
            match (self.records.get(&rk).copied(), op) {
                (None, op) => {
                    self.records.insert(rk, op);
                }
                (Some(RecOp::Put(desc)), RecOp::Release { key, generation })
                    if desc.key == key && desc.generation == generation =>
                {
                    self.records.remove(&rk);
                    let (slot, unit) = record_key_parts(&rk);
                    slots.release(slot, unit, desc.incarnation);
                }
                (Some(_), RecOp::Release { .. } | RecOp::Retired { .. }) => {}
                (Some(_), op) => {
                    self.records.insert(rk, op);
                }
            }
        }
        self.delta += later.delta;
    }
}

/// Root set of the newest durable superblock.
#[derive(Clone, Copy, Debug)]
pub(super) struct Durable {
    pub(super) seq: u64,
    pub(super) trees: [TreeRoot; 3],
}

/// Index state shared with reservations.
pub(super) struct State {
    pub(super) slots: Slots,
    pub(super) active: Overlay,
    pub(super) frozen: Option<Rc<Overlay>>,
    pub(super) durable: Durable,
    /// Bumped whenever `durable` or `frozen` changes.
    pub(super) epoch: u64,
    pub(super) next_generation: u64,
    /// Bumped on every index change.
    pub(super) mutations: u64,
    /// Retired log of the newest durable superblock.
    pub(super) log: Vec<Retired>,
    /// Pages of that log.
    pub(super) log_pages: Vec<NodeAddr>,
    /// Whether that log holds more than log pages and leftovers.
    pub(super) log_real: bool,
    /// Pages allocated by failed attempts or left over, to retire next.
    pub(super) carry: Vec<NodeAddr>,
    /// Lost ranges seen by lookups, per tree.
    pub(super) lost: Vec<(TreeKind, Key, Option<Key>)>,
    pub(super) invalid_copies: u64,
    /// Correction of the durable index entry count found by a scrub (a lost
    /// leaf's entries are gone without being counted).
    pub(super) count_fix: i64,
}

impl State {
    /// Record a change of `key` to `op` in the active layer. `old` is the
    /// key's current value and whether it is known. Releases a value only
    /// the active layer held.
    pub(super) fn set(&mut self, key: Key, op: IndexOp, old: Option<Option<Value>>) {
        let new_exists = matches!(op, IndexOp::Put(_));
        match self.active.index.get_mut(&key) {
            Some(entry) => {
                let previous = entry.op;
                entry.op = op;
                self.active.delta +=
                    i64::from(new_exists) - i64::from(matches!(previous, IndexOp::Put(_)));
                if let IndexOp::Put(prev) = previous {
                    let same = matches!(op, IndexOp::Put(v) if (v.slot, v.unit, v.generation) == (prev.slot, prev.unit, prev.generation));
                    if !same {
                        self.release_active(&key, &prev);
                    }
                }
            }
            None => {
                let existed = old.flatten().is_some();
                self.active.index.insert(key, IndexEntry { op });
                self.active.delta += i64::from(new_exists) - i64::from(existed);
            }
        }
        self.mutations += 1;
    }

    /// Free a record only the active layer referenced.
    fn release_active(&mut self, key: &Key, value: &Value) {
        let rk = record_key(value.slot, value.unit);
        if let Some(RecOp::Put(desc)) = self.active.records.get(&rk).copied()
            && desc.key == *key
            && desc.generation == value.generation
        {
            self.active.records.remove(&rk);
            self.slots.release(value.slot, value.unit, desc.incarnation);
        } else {
            // In the frozen layer or on disk: the commit releases it.
            self.active.records.entry(rk).or_insert(RecOp::Release {
                key: *key,
                generation: value.generation,
            });
        }
    }

    /// Value of `key` in the overlays: `Some(None)` for a delete, `None`
    /// when neither layer changed it.
    pub(super) fn overlay_get(&self, key: &Key) -> Option<Option<Value>> {
        let entry = self
            .active
            .index
            .get(key)
            .or_else(|| self.frozen.as_ref().and_then(|f| f.index.get(key)))?;
        Some(match entry.op {
            IndexOp::Put(value) => Some(value),
            IndexOp::Delete => None,
        })
    }

    pub(super) fn note_lost(&mut self, kind: TreeKind, low: Key, high: Option<Key>) {
        if !self
            .lost
            .iter()
            .any(|(k, l, h)| *k == kind && *l == low && *h == high)
        {
            self.lost.push((kind, low, high));
        }
    }

    fn dirty_bytes(&self) -> usize {
        (self.active.entries() + self.frozen.as_ref().map_or(0, |f| f.entries()))
            * OVERLAY_ENTRY_BYTES
    }
}

/// State reachable from reservations.
pub(super) struct Shared {
    pub(super) state: RefCell<State>,
    /// Reservations dropped while the state was borrowed.
    aborts: RefCell<Vec<Allocation>>,
}

impl Shared {
    /// Borrow the state, first aborting reservations dropped earlier.
    pub(super) fn state(&self) -> RefMut<'_, State> {
        let mut state = self.state.borrow_mut();
        for allocation in self.aborts.borrow_mut().drain(..) {
            state.slots.abort(allocation);
        }
        state
    }
}

/// Wakeups for commit waiters, and the count of metadata writes in flight.
#[derive(Default)]
pub(super) struct Signals {
    pub(super) meta_writes: Cell<usize>,
    waiters: RefCell<Vec<Waker>>,
}

impl Signals {
    pub(super) fn register(&self, waker: &Waker) {
        let mut waiters = self.waiters.borrow_mut();
        if !waiters.iter().any(|w| w.will_wake(waker)) {
            waiters.push(waker.clone());
        }
    }

    pub(super) fn wake(&self) {
        let waiters = std::mem::take(&mut *self.waiters.borrow_mut());
        for waker in waiters {
            waker.wake();
        }
    }
}

/// Held by a metadata write until its completion.
pub(super) struct MetaWrite(Rc<Signals>);

impl MetaWrite {
    pub(super) fn new(signals: &Rc<Signals>) -> Self {
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

pub(super) struct Failure {
    pub(super) attempt: u64,
    pub(super) covered: u64,
    pub(super) error: Box<dyn Any>,
}

pub(super) struct CommitState {
    pub(super) running: bool,
    pub(super) attempts: u64,
    pub(super) finished: u64,
    pub(super) succeeded: u64,
    pub(super) failed: u64,
    pub(super) durable_mutations: u64,
    pub(super) failure: Option<Failure>,
    pub(super) seq: u64,
    pub(super) durable_unit: u16,
    pub(super) pages_written: u64,
    pub(super) writes: u64,
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
                Ok(mut state) => state.slots.abort(allocation),
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

pub(super) fn class_bytes(class: u8) -> u64 {
    u64::from(class_units(class).unwrap_or(0)) * UNIT_BYTES as u64
}

/// Run futures with at most `limit` in flight. Returns outputs in completion
/// order.
pub(super) async fn bounded<'a, T: 'a>(
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

/// One layer's view of a key during a merged scan.
pub(super) enum Layer<V> {
    Set(V),
    Remove,
    Pass,
}

/// Merge sorted tree entries with two overlay layers (later wins) and push
/// the visible entries to `out` until it holds `limit`. Returns whether the
/// limit was reached.
pub(super) fn merge_layers<V: Copy>(
    tree: impl Iterator<Item = (Key, V)>,
    frozen: Vec<(Key, Layer<V>)>,
    active: Vec<(Key, Layer<V>)>,
    out: &mut Vec<(Key, V)>,
    limit: usize,
) -> bool {
    let mut tree = tree.peekable();
    let mut frozen = frozen.into_iter().peekable();
    let mut active = active.into_iter().peekable();
    loop {
        if out.len() >= limit {
            return true;
        }
        let next = [
            tree.peek().map(|e| e.0),
            frozen.peek().map(|e| e.0),
            active.peek().map(|e| e.0),
        ]
        .into_iter()
        .flatten()
        .min();
        let Some(key) = next else {
            return false;
        };
        let mut value = None;
        if tree.peek().is_some_and(|e| e.0 == key) {
            value = tree.next().map(|e| e.1);
        }
        for layer in [&mut frozen, &mut active] {
            if layer.peek().is_some_and(|e| e.0 == key) {
                match layer.next().expect("peeked").1 {
                    Layer::Set(v) => value = Some(v),
                    Layer::Remove => value = None,
                    Layer::Pass => {}
                }
            }
        }
        if let Some(value) = value {
            out.push((key, value));
        }
    }
}

fn index_layer(entry: &IndexEntry) -> Layer<Value> {
    match entry.op {
        IndexOp::Put(value) => Layer::Set(value),
        IndexOp::Delete => Layer::Remove,
    }
}

fn record_layer(op: &RecOp) -> Layer<RecordDesc> {
    match op {
        RecOp::Put(desc) => Layer::Set(*desc),
        RecOp::Release { .. } | RecOp::Retired { .. } => Layer::Pass,
    }
}

fn bound_key(bound: &Bound<Key>) -> Key {
    match bound {
        Bound::Included(key) | Bound::Excluded(key) => *key,
        Bound::Unbounded => Key::MIN,
    }
}

fn after_low(key: &Key, low: &Bound<Key>) -> bool {
    match low {
        Bound::Included(l) => key >= l,
        Bound::Excluded(l) => key > l,
        Bound::Unbounded => true,
    }
}

fn before_high(key: &Key, high: &Bound<Key>) -> bool {
    match high {
        Bound::Included(h) => key <= h,
        Bound::Excluded(h) => key < h,
        Bound::Unbounded => true,
    }
}

/// Extent volume. See the module docs.
pub struct Volume<C: Charge> {
    pub(super) slab: Rc<Slab<C>>,
    pub(super) charge: ChargeFn<C>,
    pub(super) alignment: Alignment,
    pub(super) slot_count: u64,
    pub(super) volume_id: Cell<[u8; 16]>,
    pub(super) shared: Rc<Shared>,
    pub(super) commit: RefCell<CommitState>,
    pub(super) signals: Rc<Signals>,
    pub(super) pager: Rc<Pager>,
    pub(super) dirty_limit: usize,
    /// Commit run buffers kept between writes so small commits do not
    /// allocate, zero, and free a full run each time. Each keeps its charge.
    pub(super) run_buffers: RefCell<Vec<AlignedBuffer<C>>>,
    // Index entries removed by eviction and not yet handed to a caller.
    pub(super) evicted: RefCell<Vec<(Key, Value)>>,
}

/// Page source for tree code: the pager plus the I/O context of one call.
pub(super) struct Io<'a, C: Charge, S: Scope, B: Budget> {
    pub(super) volume: &'a Volume<C>,
    pub(super) reactor: &'a Reactor<S, B>,
    pub(super) scope: &'a S,
}

impl<C: Charge, S: Scope, B: Budget> btree::Source for Io<'_, C, S, B>
where
    S::Error: From<Error>,
{
    type Error = S::Error;

    fn cached(&self, req: &Req) -> Option<Rc<TreePage>> {
        req.addrs.iter().filter(|a| !a.is_none()).find_map(|addr| {
            self.volume
                .pager
                .get(*addr, req.priority)
                .filter(|page| fits(page, req))
        })
    }

    fn load(
        &self,
        req: Req,
    ) -> LocalFuture<'_, std::result::Result<Option<Rc<TreePage>>, S::Error>> {
        Box::pin(async move { self.volume.load_page(self.reactor, self.scope, req).await })
    }

    fn prefetch(&self, reqs: Vec<Req>) -> LocalFuture<'_, ()> {
        Box::pin(async move {
            let missing: Vec<Req> = reqs
                .into_iter()
                .filter(|req| !self.volume.pager.contains(req.addrs[0]))
                .collect();
            bounded(
                missing.into_iter().map(|req| {
                    Box::pin(async move {
                        let _ = self.volume.load_page(self.reactor, self.scope, req).await;
                    }) as Pin<Box<dyn Future<Output = ()> + '_>>
                }),
                IO_CONCURRENCY,
            )
            .await;
        })
    }
}

fn fits(page: &TreePage, req: &Req) -> bool {
    page.fits(
        req.kind,
        req.level,
        req.max_seq,
        &req.low,
        req.high.as_ref(),
    )
}

impl<C: Charge> Volume<C> {
    pub(super) fn new_unmounted(
        slab: Rc<Slab<C>>,
        alignment: Alignment,
        slot_count: u64,
        options: &VolumeOptions<C>,
    ) -> Result<Self> {
        let dirty_limit = if options.dirty_bytes == 0 {
            (options.metadata_cache_bytes / 4).max(DEFAULT_DIRTY_BYTES)
        } else {
            options.dirty_bytes
        };
        Ok(Self {
            slab,
            charge: Rc::clone(&options.charge),
            alignment,
            slot_count,
            volume_id: Cell::new(options.volume_id),
            shared: Rc::new(Shared {
                state: RefCell::new(State {
                    slots: Slots::new(slot_count, options.limits)?,
                    active: Overlay::default(),
                    frozen: None,
                    durable: Durable {
                        seq: 0,
                        trees: [TreeRoot::EMPTY; 3],
                    },
                    epoch: 0,
                    next_generation: 1,
                    mutations: 0,
                    log: Vec::new(),
                    log_pages: Vec::new(),
                    log_real: false,
                    carry: Vec::new(),
                    lost: Vec::new(),
                    invalid_copies: 0,
                    count_fix: 0,
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
                durable_unit: super::format::SUPERBLOCK_A_UNIT,
                pages_written: 0,
                writes: 0,
            }),
            signals: Rc::new(Signals::default()),
            pager: Pager::new(options.metadata_cache_bytes),
            dirty_limit,
            run_buffers: RefCell::new(Vec::new()),
            evicted: RefCell::new(Vec::new()),
        })
    }

    pub(super) fn io<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Io<'a, C, S, B> {
        Io {
            volume: self,
            reactor,
            scope,
        }
    }

    /// Read one unit, retrying I/O errors.
    pub(super) async fn read_unit<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        offset: u64,
        scope: &S,
    ) -> std::result::Result<Box<[u8; UNIT_BYTES]>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut last = None;
        for _ in 0..READ_ATTEMPTS {
            let buffer = self.slab.allocate(UNIT_BYTES, (self.charge)(UNIT_BYTES)?)?;
            match self.slab.read_at(reactor, offset, buffer, (), scope).await {
                Ok(buffer) => {
                    let mut bytes = Box::new([0u8; UNIT_BYTES]);
                    bytes.copy_from_slice(buffer.as_slice());
                    return Ok(bytes);
                }
                Err(error) => last = Some(error),
            }
        }
        Err(last.expect("a read failed"))
    }

    /// A page copy that fits `req`, through the pager.
    pub(super) async fn load_page<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        req: Req,
    ) -> std::result::Result<Option<Rc<TreePage>>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut io_error = None;
        let mut invalid = 0;
        for addr in req.addrs.iter().copied().filter(|a| !a.is_none()) {
            if let Some(page) = self.pager.get(addr, req.priority) {
                if fits(&page, &req) {
                    if invalid != 0 {
                        self.shared.state().invalid_copies += 1;
                    }
                    return Ok(Some(page));
                }
                invalid += 1;
                continue;
            }
            let found = loop {
                match self.pager.begin_fault(addr) {
                    FaultRole::Hit(page) => break Some(page),
                    FaultRole::Leader(guard) => {
                        match self.read_unit(reactor, addr.offset(), scope).await {
                            Ok(bytes) => {
                                let page = TreePage::decode(
                                    bytes,
                                    &self.volume_id.get(),
                                    addr,
                                    self.slot_count,
                                );
                                match guard.finish(page, req.priority) {
                                    FaultResult::Page(page) => break Some(page),
                                    _ => break None,
                                }
                            }
                            Err(error) => {
                                drop(guard);
                                io_error = Some(error);
                                break None;
                            }
                        }
                    }
                    FaultRole::Follower(follower) => match follower.wait().await {
                        FaultResult::Page(page) => break Some(page),
                        FaultResult::Invalid => break None,
                        FaultResult::Retry => continue,
                    },
                }
            };
            match found {
                Some(page) if fits(&page, &req) => {
                    if invalid != 0 {
                        self.shared.state().invalid_copies += 1;
                    }
                    return Ok(Some(page));
                }
                _ => invalid += 1,
            }
        }
        match io_error {
            Some(error) => Err(error),
            None => Ok(None),
        }
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
        self.volume_id.get()
    }

    /// Entries in the index. Exact when every overlay change knew whether
    /// its key existed (always after a commit); a publish that replaced an
    /// entry whose page was not cached counts as an insert until then.
    pub fn len(&self) -> u64 {
        let state = self.shared.state();
        let delta =
            state.active.delta + state.frozen.as_ref().map_or(0, |f| f.delta) + state.count_fix;
        (state.durable.trees[TreeKind::Index.index()].count as i64 + delta).max(0) as u64
    }

    /// Whether the index has no entries.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Whether a commit has work to do: index changes that are not durable
    /// yet, slot table changes, or retired space that the next commit
    /// releases.
    pub fn is_dirty(&self) -> bool {
        let state = self.shared.state();
        !state.active.is_empty()
            || state.frozen.is_some()
            || state.slots.has_ops()
            || state.log_real
            || state.mutations > self.commit.borrow().durable_mutations
    }

    /// Whether uncommitted changes reached the dirty limit. Allocation
    /// returns `Busy` until a commit drains them.
    pub fn needs_commit(&self) -> bool {
        self.shared.state().dirty_bytes() >= self.dirty_limit
    }

    /// Bytes of uncommitted index changes held in memory.
    pub fn dirty_bytes(&self) -> usize {
        self.shared.state().dirty_bytes()
    }

    /// Metadata page cache counters.
    pub fn pager_stats(&self) -> PagerStats {
        self.pager.stats()
    }

    /// Change the metadata page cache budget.
    pub fn set_metadata_cache_bytes(&self, bytes: usize) {
        self.pager.set_budget(bytes);
    }

    /// Key ranges `[low, high)` of the index that lookups found lost since
    /// mount.
    pub fn lost_ranges(&self) -> Vec<(Key, Option<Key>)> {
        self.shared
            .state()
            .lost
            .iter()
            .filter(|(kind, _, _)| *kind == TreeKind::Index)
            .map(|(_, low, high)| (*low, *high))
            .collect()
    }

    /// Value of `key` from memory only: the overlays and cached pages.
    /// `None` when answering needs a page read.
    pub fn try_get(&self, key: &Key) -> Option<Option<Value>> {
        let state = self.shared.state();
        if let Some(value) = state.overlay_get(key) {
            return Some(value);
        }
        let durable = state.durable;
        drop(state);
        let io = TryIo(self);
        match btree::try_get(
            &io,
            &durable.trees[TreeKind::Index.index()],
            TreeKind::Index,
            durable.seq,
            key,
        )? {
            Lookup::Found(raw) => Some(Value::decode(raw[..32].try_into().expect("32"))),
            Lookup::Absent | Lookup::Lost(..) => Some(None),
        }
    }

    /// Values of `keys` (sorted ascending) from memory only, like
    /// [`Volume::try_get`] for each, sharing the tree descent.
    pub fn try_get_many(&self, keys: &[Key]) -> Vec<Option<Option<Value>>> {
        let state = self.shared.state();
        let durable = state.durable;
        let mut out: Vec<Option<Option<Value>>> =
            keys.iter().map(|k| state.overlay_get(k)).collect();
        drop(state);
        let pending: Vec<usize> = (0..keys.len()).filter(|&i| out[i].is_none()).collect();
        if pending.is_empty() {
            return out;
        }
        let tree_keys: Vec<Key> = pending.iter().map(|&i| keys[i]).collect();
        let found = btree::try_get_many(
            &TryIo(self),
            &durable.trees[TreeKind::Index.index()],
            TreeKind::Index,
            durable.seq,
            &tree_keys,
        );
        for (&i, lookup) in pending.iter().zip(found) {
            out[i] = lookup.map(|lookup| match lookup {
                Lookup::Found(raw) => Value::decode(raw[..32].try_into().expect("32")),
                Lookup::Absent | Lookup::Lost(..) => None,
            });
        }
        out
    }

    /// Current value of `key`, reading pages as needed. Ranges whose page
    /// is lost read as empty and are listed by [`Volume::lost_ranges`].
    pub fn get<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        key: &Key,
        scope: &'a S,
    ) -> Operation<'a, Option<Value>, S::Error>
    where
        S::Error: From<Error>,
    {
        let key = *key;
        Box::pin(async move { self.resolve(reactor, scope, &key).await })
    }

    /// Current value of `key`. When this returns, the value is still current
    /// (no await happens after the check).
    pub(super) async fn resolve<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        key: &Key,
    ) -> std::result::Result<Option<Value>, S::Error>
    where
        S::Error: From<Error>,
    {
        // Fast path: overlays and cached pages, no future per level.
        if let Some(value) = self.try_get(key) {
            return Ok(value);
        }
        loop {
            let (durable, epoch) = {
                let state = self.shared.state();
                if let Some(value) = state.overlay_get(key) {
                    return Ok(value);
                }
                (state.durable, state.epoch)
            };
            let io = self.io(reactor, scope);
            let found = btree::get(
                &io,
                &durable.trees[TreeKind::Index.index()],
                TreeKind::Index,
                durable.seq,
                key,
            )
            .await?;
            let mut state = self.shared.state();
            if state.epoch != epoch || state.overlay_get(key).is_some() {
                continue;
            }
            return Ok(match found {
                Lookup::Found(raw) => Value::decode(raw[..32].try_into().expect("32")),
                Lookup::Absent => None,
                Lookup::Lost(low, high) => {
                    state.note_lost(TreeKind::Index, low, high);
                    None
                }
            });
        }
    }

    /// Entries with keys in `(low, high)`, in key order, at most `limit`.
    pub fn range<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        low: Bound<Key>,
        high: Bound<Key>,
        limit: usize,
        scope: &'a S,
    ) -> Operation<'a, Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move { self.scan_index(reactor, scope, low, high, limit).await })
    }

    /// Up to `limit` entries with keys after `after` (from the start if
    /// `None`), in key order.
    pub fn next_after<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        after: Option<&Key>,
        limit: usize,
        scope: &'a S,
    ) -> Operation<'a, Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        let low = after.map_or(Bound::Unbounded, |key| Bound::Excluded(*key));
        Box::pin(async move {
            self.scan_index(reactor, scope, low, Bound::Unbounded, limit)
                .await
        })
    }

    pub(super) async fn scan_index<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        low: Bound<Key>,
        high: Bound<Key>,
        limit: usize,
    ) -> std::result::Result<Vec<(Key, Value)>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut out = Vec::new();
        self.scan(
            reactor,
            scope,
            TreeKind::Index,
            low,
            high,
            limit,
            &mut out,
            |raw| Value::decode(raw[..32].try_into().expect("32")),
            |overlay, low, high| {
                overlay
                    .index
                    .range((low, high))
                    .map(|(k, e)| (*k, index_layer(e)))
                    .collect()
            },
        )
        .await?;
        Ok(out)
    }

    pub(super) async fn scan_records<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        low: Bound<Key>,
        high: Bound<Key>,
        limit: usize,
    ) -> std::result::Result<Vec<(Key, RecordDesc)>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut out = Vec::new();
        self.scan(
            reactor,
            scope,
            TreeKind::Records,
            low,
            high,
            limit,
            &mut out,
            RecordDesc::decode,
            |overlay, low, high| {
                overlay
                    .records
                    .range((low, high))
                    .map(|(k, op)| (*k, record_layer(op)))
                    .collect()
            },
        )
        .await?;
        Ok(out)
    }

    /// Merged scan of one tree and the overlays, one leaf at a time.
    #[allow(clippy::too_many_arguments)]
    async fn scan<S: Scope, B: Budget, V: Copy>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        kind: TreeKind,
        low: Bound<Key>,
        high: Bound<Key>,
        limit: usize,
        out: &mut Vec<(Key, V)>,
        decode: impl Fn(&[u8]) -> Option<V>,
        layer: impl Fn(&Overlay, Bound<Key>, Bound<Key>) -> Vec<(Key, Layer<V>)>,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let mut cursor = low;
        while out.len() < limit {
            let (durable, epoch) = {
                let state = self.shared.state();
                (state.durable, state.epoch)
            };
            let start = bound_key(&cursor);
            if !before_high(&start, &high) && !matches!(cursor, Bound::Unbounded) {
                break;
            }
            let io = self.io(reactor, scope);
            let seek = btree::seek(
                &io,
                &durable.trees[kind.index()],
                kind,
                durable.seq,
                &start,
                Priority::Scan,
            )
            .await?;
            let mut state = self.shared.state();
            if state.epoch != epoch {
                continue;
            }
            let (entries, leaf_high): (Vec<(Key, V)>, Option<Key>) = match seek {
                Seek::Empty => (Vec::new(), None),
                Seek::Lost(l, h) => {
                    state.note_lost(kind, l, h);
                    (Vec::new(), h)
                }
                Seek::Leaf(page, h) => (
                    (0..page.count())
                        .filter_map(|i| decode(page.value(i)).map(|v| (page.key(i), v)))
                        .collect(),
                    h,
                ),
            };
            // The interval [cursor, end) this leaf answers.
            let end = match (leaf_high, &high) {
                (Some(h), Bound::Unbounded) => Bound::Excluded(h),
                (Some(h), Bound::Included(x)) if h <= *x => Bound::Excluded(h),
                (Some(h), Bound::Excluded(x)) if h < *x => Bound::Excluded(h),
                (_, other) => *other,
            };
            let in_range = |key: &Key| after_low(key, &cursor) && before_high(key, &end);
            let tree = entries.into_iter().filter(|(k, _)| in_range(k));
            let frozen = state
                .frozen
                .as_ref()
                .map(|f| layer(f, cursor, end))
                .unwrap_or_default();
            let active = layer(&state.active, cursor, end);
            drop(state);
            if merge_layers(tree, frozen, active, out, limit) {
                break;
            }
            match (leaf_high, end) {
                (Some(h), Bound::Excluded(e)) if h == e => cursor = Bound::Included(h),
                _ => break,
            }
        }
        Ok(())
    }

    /// Entries with keys in `(low, high)`, at most `limit`, from memory only:
    /// the overlays and cached pages. `None` when a page would have to be
    /// read; [`Volume::prefetch`] loads it.
    pub fn try_range(
        &self,
        low: Bound<Key>,
        high: Bound<Key>,
        limit: usize,
    ) -> Option<Vec<(Key, Value)>> {
        let mut out = Vec::new();
        let mut cursor = low;
        let state = self.shared.state();
        let durable = state.durable;
        let io = TryIo(self);
        while out.len() < limit {
            let start = bound_key(&cursor);
            if !before_high(&start, &high) && !matches!(cursor, Bound::Unbounded) {
                break;
            }
            let seek = btree::try_seek(
                &io,
                &durable.trees[TreeKind::Index.index()],
                TreeKind::Index,
                durable.seq,
                &start,
            )?;
            let (entries, leaf_high): (Vec<(Key, Value)>, Option<Key>) = match seek {
                Seek::Empty => (Vec::new(), None),
                Seek::Lost(_, h) => (Vec::new(), h),
                Seek::Leaf(page, h) => (
                    (0..page.count())
                        .filter_map(|i| {
                            Value::decode(page.value(i).try_into().ok()?).map(|v| (page.key(i), v))
                        })
                        .collect(),
                    h,
                ),
            };
            let end = match (leaf_high, &high) {
                (Some(h), Bound::Unbounded) => Bound::Excluded(h),
                (Some(h), Bound::Included(x)) if h <= *x => Bound::Excluded(h),
                (Some(h), Bound::Excluded(x)) if h < *x => Bound::Excluded(h),
                (_, other) => *other,
            };
            let in_range = |key: &Key| after_low(key, &cursor) && before_high(key, &end);
            let tree = entries.into_iter().filter(|(k, _)| in_range(k));
            let layer = |overlay: &Overlay| -> Vec<(Key, Layer<Value>)> {
                overlay
                    .index
                    .range((cursor, end))
                    .map(|(k, e)| (*k, index_layer(e)))
                    .collect()
            };
            let frozen = state.frozen.as_ref().map(|f| layer(f)).unwrap_or_default();
            let active = layer(&state.active);
            if merge_layers(tree, frozen, active, &mut out, limit) {
                break;
            }
            match (leaf_high, end) {
                (Some(h), Bound::Excluded(e)) if h == e => cursor = Bound::Included(h),
                _ => break,
            }
        }
        Some(out)
    }

    /// Load the index pages on the path to `key` into the cache, so memory
    /// only calls ([`Volume::try_get`], [`Volume::try_range`]) can answer.
    pub fn prefetch<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        key: &Key,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        let key = *key;
        Box::pin(async move {
            let durable = self.shared.state().durable;
            let io = self.io(reactor, scope);
            btree::seek(
                &io,
                &durable.trees[TreeKind::Index.index()],
                TreeKind::Index,
                durable.seq,
                &key,
                Priority::Leaf,
            )
            .await?;
            Ok(())
        })
    }

    /// Pin a slot so eviction skips it. `None` when out of range.
    pub fn lease(&self, slot: u32) -> Option<SlotLease> {
        (u64::from(slot) < self.slot_count).then(|| self.shared.state().slots.lease(slot))
    }

    /// Read a record into `buffer`, from `value.offset()` for `buffer.len()`
    /// bytes. The buffer must follow [`Volume::alignment`] and fit the
    /// record's class, or this fails with `InvalidConfiguration`.
    ///
    /// The slot is leased while the read runs and its CLOCK reference bit is
    /// set. The slot may have been evicted and reused since the lookup, so
    /// the caller must check the record header (key and generation).
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
    /// `SLOT_BYTES`, and with `Busy` when there is no free or open slot, the
    /// storage quota is reached, the small-record byte cap would be exceeded,
    /// or uncommitted changes reached the dirty limit ([`Volume::needs_commit`]).
    pub fn allocate(&self, tier: Tier, length: u64) -> Result<Reservation> {
        let mut state = self.shared.state();
        if length == 0 || length > SLOT_BYTES {
            return Err(Error::InvalidConfiguration);
        }
        if state.dirty_bytes() >= self.dirty_limit {
            return Err(Error::Busy);
        }
        let allocation = state.slots.allocate(tier, length)?;
        drop(state);
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

    /// A unique, nonzero write id, larger than any id handed out or committed
    /// before on this volume. Mount skips `GENERATION_SKIP` ids past the
    /// committed high-water mark; the skip is durable after the first commit
    /// following the mount.
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

    /// Insert `key` for the written reservation. The previous value of `key`
    /// (if any) is released: at once when it was never committed, otherwise
    /// by the next commit (cache) or the commit after it (storage).
    ///
    /// `generation` must be the id in the record header and `length` the bytes
    /// written. Fails with `InvalidConfiguration` when the reservation was not
    /// written, `length` is 0 or above its capacity, or `generation` is 0,
    /// and with `Busy` while a write to it is in flight. On error the
    /// reservation is dropped, which frees it.
    pub fn publish(
        &self,
        key: Key,
        reservation: Reservation,
        length: u32,
        generation: u64,
        retain_until: u64,
    ) -> Result<()> {
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
        let known = self.try_get(&key);
        let mut state = self.shared.state();
        let incarnation = allocation.incarnation;
        state.slots.published(allocation);
        state.active.records.insert(
            record_key(value.slot, value.unit),
            RecOp::Put(RecordDesc {
                key,
                generation,
                incarnation,
            }),
        );
        state.set(key, IndexOp::Put(value), known);
        if retain_until != 0 && value.tier == Tier::Storage {
            state.slots.retain(value.slot, retain_until);
        }
        Ok(())
    }

    /// Commit first when the overlay is at the dirty limit, so mutation
    /// paths keep uncommitted changes bounded by `dirty_bytes`.
    pub(super) async fn throttle<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        if self.needs_commit() {
            self.commit(reactor, scope).await?;
        }
        Ok(())
    }

    /// Remove `key`. Returns its value. Commits first when the dirty limit
    /// is reached.
    pub fn remove<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        key: &Key,
        scope: &'a S,
    ) -> Operation<'a, Option<Value>, S::Error>
    where
        S::Error: From<Error>,
    {
        let key = *key;
        Box::pin(async move {
            self.throttle(reactor, scope).await?;
            let old = self.resolve(reactor, scope, &key).await?;
            if old.is_some() {
                self.shared.state().set(key, IndexOp::Delete, Some(old));
            }
            Ok(old)
        })
    }

    /// Remove `key` only if its value is `expected`. Returns whether it was
    /// removed. Commits first when the dirty limit is reached.
    pub fn remove_if<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        key: &Key,
        expected: &Value,
        scope: &'a S,
    ) -> Operation<'a, bool, S::Error>
    where
        S::Error: From<Error>,
    {
        let (key, expected) = (*key, *expected);
        Box::pin(async move {
            self.throttle(reactor, scope).await?;
            let old = self.resolve(reactor, scope, &key).await?;
            if old != Some(expected) {
                return Ok(false);
            }
            self.shared.state().set(key, IndexOp::Delete, Some(old));
            Ok(true)
        })
    }

    /// Remove `key` if its value is `expected`, from memory only. Returns
    /// whether it was removed; `false` also when answering needs a page read.
    pub fn remove_cached(&self, key: &Key, expected: &Value) -> bool {
        if self.try_get(key) != Some(Some(*expected)) {
            return false;
        }
        self.shared
            .state()
            .set(*key, IndexOp::Delete, Some(Some(*expected)));
        true
    }

    /// Remove every entry with a key in `(low, high)`, a batch at a time.
    /// Hands each batch of removed entries, in key order, to `on_removed`,
    /// and commits between batches when the dirty limit is reached, so a
    /// large range never holds more than `dirty_bytes` of changes. Returns
    /// the count removed. On an error the batches already handed over stay
    /// removed.
    pub fn remove_range<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        low: Bound<Key>,
        high: Bound<Key>,
        mut on_removed: impl FnMut(&[(Key, Value)]) + 'a,
        scope: &'a S,
    ) -> Operation<'a, usize, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let mut count = 0;
            let mut cursor = low;
            loop {
                self.throttle(reactor, scope).await?;
                let mut removed = Vec::new();
                let batch = self.scan_index(reactor, scope, cursor, high, 1024).await?;
                let Some(last) = batch.last().map(|(k, _)| *k) else {
                    break;
                };
                for (key, value) in batch {
                    // Only remove what is still current.
                    let mut state = self.shared.state();
                    let current = match state.overlay_get(&key) {
                        Some(current) => current,
                        None => Some(value),
                    };
                    if current == Some(value) {
                        state.set(key, IndexOp::Delete, Some(Some(value)));
                        removed.push((key, value));
                    }
                }
                count += removed.len();
                on_removed(&removed);
                cursor = Bound::Excluded(last);
            }
            Ok(count)
        })
    }

    /// Set the `retain_until` of `key`'s entry. Raising also raises its
    /// slot's bound at once; lowering leaves the slot's bound loose until
    /// [`Volume::refresh_retain`] recomputes it. Returns whether `key` was
    /// found.
    pub fn set_retain<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        key: &Key,
        until: u64,
        scope: &'a S,
    ) -> Operation<'a, bool, S::Error>
    where
        S::Error: From<Error>,
    {
        let key = *key;
        Box::pin(async move {
            self.throttle(reactor, scope).await?;
            let Some(old) = self.resolve(reactor, scope, &key).await? else {
                return Ok(false);
            };
            if old.retain_until == until {
                return Ok(true);
            }
            let mut state = self.shared.state();
            state.set(
                key,
                IndexOp::Put(Value {
                    retain_until: until,
                    ..old
                }),
                Some(Some(old)),
            );
            if old.tier == Tier::Storage {
                if until > old.retain_until {
                    state.slots.retain(old.slot, until);
                } else {
                    state.slots.retain_lowered(old.slot);
                }
            }
            Ok(true)
        })
    }

    /// Recompute the `retain_until` bound of up to `limit` storage slots
    /// whose records lowered theirs. Returns how many were recomputed.
    pub fn refresh_retain<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        limit: usize,
        scope: &'a S,
    ) -> Operation<'a, usize, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let stale = self.shared.state().slots.stale_retain(limit);
            let mut done = 0;
            for (slot, incarnation) in stale {
                let Some(seen) = self
                    .shared
                    .state()
                    .slots
                    .slot_state(slot)
                    .map(|s| s.retain_until)
                else {
                    continue;
                };
                let mut max = 0;
                let mut retired = false;
                for (rk, desc) in self.slot_records(reactor, scope, slot).await? {
                    if desc.incarnation != incarnation {
                        continue;
                    }
                    let (_, unit) = record_key_parts(&rk);
                    match self.resolve(reactor, scope, &desc.key).await? {
                        Some(value) if value.slot == slot && value.unit == unit => {
                            max = max.max(value.retain_until);
                        }
                        // Removed but not released: an older durable tree
                        // may still reference it with a bound we no longer
                        // know. Keep the slot's bound until it is released.
                        _ => retired = true,
                    }
                }
                if retired {
                    continue;
                }
                self.shared
                    .state()
                    .slots
                    .set_retain(slot, incarnation, seen, max);
                done += 1;
            }
            Ok(done)
        })
    }

    /// Load the record map leaves of `slot` into the cache, concurrently.
    pub(super) async fn prefetch_slot_records<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        slot: u32,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let durable = self.shared.state().durable;
        let high = (u64::from(slot) + 1 < self.slot_count).then(|| record_key(slot + 1, 0));
        btree::prefetch_range(
            &self.io(reactor, scope),
            &durable.trees[TreeKind::Records.index()],
            TreeKind::Records,
            durable.seq,
            &record_key(slot, 0),
            high.as_ref(),
        )
        .await
    }

    /// Load the index pages on the paths to `keys`, concurrently.
    pub(super) async fn prefetch_keys<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        keys: &[Key],
    ) where
        S::Error: From<Error>,
    {
        bounded(
            keys.iter().map(|key| {
                Box::pin(async move {
                    let _ = self.prefetch(reactor, key, scope).await;
                }) as Pin<Box<dyn Future<Output = ()> + '_>>
            }),
            IO_CONCURRENCY,
        )
        .await;
    }

    /// Record map entries of one slot, merged with the overlays.
    pub(super) async fn slot_records<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        slot: u32,
    ) -> std::result::Result<Vec<(Key, RecordDesc)>, S::Error>
    where
        S::Error: From<Error>,
    {
        self.prefetch_slot_records(reactor, scope, slot).await?;
        let high = if u64::from(slot) + 1 < self.slot_count {
            Bound::Excluded(record_key(slot + 1, 0))
        } else {
            Bound::Unbounded
        };
        self.scan_records(
            reactor,
            scope,
            Bound::Included(record_key(slot, 0)),
            high,
            usize::MAX,
        )
        .await
    }

    /// Occupancy summary.
    pub fn usage(&self) -> Usage {
        self.shared.state().slots.usage()
    }

    /// State of one slot, or `None` when out of range. Slot `i` covers bytes
    /// `[i * SLOT_BYTES, (i + 1) * SLOT_BYTES)` of the slab.
    pub fn slot_state(&self, slot: u32) -> Option<SlotState> {
        self.shared.state().slots.slot_state(slot)
    }

    /// Bytes in use by node units and records in slots `range`. O(log slots).
    pub fn used_bytes(&self, range: std::ops::Range<u32>) -> u64 {
        self.shared.state().slots.used_bytes(range)
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
            pages_written: commit.pages_written,
            writes: commit.writes,
        }
    }
}

/// Cache-only page source for [`Volume::try_get`].
struct TryIo<'a, C: Charge>(&'a Volume<C>);

impl<C: Charge> btree::Source for TryIo<'_, C> {
    type Error = std::convert::Infallible;

    fn cached(&self, req: &Req) -> Option<Rc<TreePage>> {
        req.addrs.iter().filter(|a| !a.is_none()).find_map(|addr| {
            self.0
                .pager
                .get(*addr, req.priority)
                .filter(|p| fits(p, req))
        })
    }

    fn load(
        &self,
        _req: Req,
    ) -> LocalFuture<'_, std::result::Result<Option<Rc<TreePage>>, Self::Error>> {
        Box::pin(async { Ok(None) })
    }

    fn prefetch(&self, _reqs: Vec<Req>) -> LocalFuture<'_, ()> {
        Box::pin(async {})
    }
}
