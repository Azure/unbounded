//! Deterministic simulation of allocator lifecycles through the public API.
//!
//! One operation vocabulary, one seeded generator, and one runner drive fixed
//! regression traces, a restore cross-product, and random sequences. The runner
//! keeps an independent record model: every append that reuses bytes supersedes
//! earlier records there, and a superseded record must never validate or lease
//! again, whatever the allocator's generations say. Restore, append, reclaim,
//! and I/O results are also checked against small executable specifications.
//!
//! Replay one random case with `DST_SEED=<seed>`; tune with `DST_SEEDS` and
//! `DST_STEPS`. A failure prints the case, step, and operation.

#![cfg(feature = "simulation")]

use page_alloc::{
    AlignedBuffer, Charge, Error, Extent, FreezeGuard, Generation, SegmentClock, SegmentEntries,
    SegmentId, SegmentLease, SegmentSnapshot, SegmentState, Segments, Slab,
};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    rc::Rc,
    task::{Context, Poll, Waker},
};
use uring_runtime::{
    Operation, Scope,
    reactor::{
        Reactor,
        simulation::{Fault, Simulation},
    },
};

const PAGE: u64 = 4096;
const SEGMENT: u64 = 2 * PAGE;
const SLOTS: usize = 3;
const CAPACITY: u64 = SEGMENT * SLOTS as u64;
const PATH: &str = "/dst/slab";
const HISTORY: usize = 32;
const MAX_LEASES: usize = 8;
const MAX_BUFFERS: usize = 4;
const TURNS: usize = 200;

/// Allocator and runtime errors kept distinct for completion assertions.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TestError {
    Alloc(Error),

    Runtime(uring_runtime::Error),
}

impl From<Error> for TestError {
    /// Keep allocator categories visible.
    fn from(error: Error) -> Self {
        Self::Alloc(error)
    }
}

impl From<uring_runtime::Error> for TestError {
    /// Keep runtime categories visible.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

/// An always-live scope; cancellation comes from dropping futures.
#[derive(Clone)]
struct TestScope;

impl Scope for TestScope {
    type Error = TestError;

    /// Never cancel from the scope.
    fn check(&self) -> Result<(), TestError> {
        Ok(())
    }
}

static SCOPE: TestScope = TestScope;

/// Caller accounting that must balance against live and idle buffers.
struct Counted {
    used: Rc<Cell<usize>>,

    bytes: usize,
}

impl Counted {
    /// Admit bytes before the allocator takes ownership of the guard.
    fn new(used: &Rc<Cell<usize>>, bytes: usize) -> Self {
        used.set(used.get() + bytes);
        Self {
            used: used.clone(),
            bytes,
        }
    }
}

impl Charge for Counted {
    /// Cover only admitted bytes.
    fn covers(&self, bytes: usize) -> bool {
        self.bytes >= bytes
    }
}

impl Drop for Counted {
    /// Return admission when the last owner releases the guard.
    fn drop(&mut self) {
        self.used.set(self.used.get() - self.bytes);
    }
}

/// Stable splitmix64 so seeds replay identically on every platform.
struct Rng(u64);

impl Rng {
    /// Next 64 pseudo-random bits.
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }

    /// Uniform-enough value below a small bound.
    fn below(&mut self, bound: u64) -> u64 {
        self.next() % bound
    }

    /// Small byte below a bound.
    fn byte(&mut self, bound: u64) -> u8 {
        self.below(bound) as u8
    }

    /// True with the given percentage.
    fn chance(&mut self, percent: u64) -> bool {
        self.below(100) < percent
    }
}

/// How an appended reservation is used.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Io {
    /// Write and drive to completion.
    Complete,

    /// Write, poll once, and keep the future.
    Pending,

    /// Write, poll once, and drop the future if it has not completed.
    Abandon,

    /// Create the write future without polling it; it still owns the lease.
    Unpolled,

    /// Keep the append lease without writing.
    Lease,

    /// Drop the append lease without writing.
    Skip,
}

/// Fault injected for the next read or write submission.
#[derive(Clone, Copy, Debug)]
enum FaultKind {
    Errno,

    Short,

    Delay(u8),

    Hold(u8),

    Reject,
}

impl FaultKind {
    /// Whether the faulted operation must fail, succeed, or may do either.
    fn fails(self) -> Option<bool> {
        match self {
            Self::Errno | Self::Short => Some(true),
            Self::Delay(_) | Self::Hold(_) => Some(false),
            Self::Reject => None,
        }
    }
}

/// Generation chosen relative to the live slot generation.
#[derive(Clone, Copy, Debug)]
enum Gen {
    Zero,

    Lower,

    Equal,

    Higher,

    NearMax,

    Max,
}

/// One slot change applied to a restore image.
#[derive(Clone, Copy, Debug)]
struct Edit {
    slot: u8,

    state: Option<SegmentState>,

    generation: Option<Gen>,

    used: Option<u64>,

    wrong_id: bool,
}

impl Edit {
    /// Edit one slot's state, generation, and occupancy.
    fn slot(slot: u8, state: SegmentState, generation: Gen, used: u64) -> Self {
        Self {
            slot,
            state: Some(state),
            generation: Some(generation),
            used: Some(used),
            wrong_id: false,
        }
    }
}

/// Source of a restore image.
#[derive(Clone, Debug)]
enum Image {
    /// A retained snapshot; zero is the newest.
    History(u8),

    /// The live table's own snapshot.
    Current,

    /// The live snapshot missing its last slot.
    Truncated,

    /// A history or live snapshot with slot edits.
    Edited { base: Option<u8>, edits: Vec<Edit> },
}

/// The single operation vocabulary shared by every DST case.
#[derive(Clone, Debug)]
enum Op {
    Append {
        pages: u8,
        io: Io,
        fault: Option<FaultKind>,
    },
    Read {
        pick: u8,
        fault: Option<FaultKind>,
        abandon: bool,
    },
    ReadAll,
    Turn(u8),
    Abandon(u8),
    Lease(u8),
    Release(u8),
    Snapshot,
    Restore {
        image: Image,
        quiesce: bool,
    },
    ExpectRestore(Result<(), Error>),
    Freeze,
    Thaw,
    Reclaim {
        reserve: u8,
        visits: u8,
        entries: u8,
    },
    Scored {
        reserve: u8,
        visits: u8,
        entries: u8,
        salt: u8,
    },
    ReclaimIndex {
        visits: u8,
        keep: u8,
    },
    MarkRead(u8),
    Evict(u8),
    Recycle(u8),
    Buffer(u8),
    DropBuffer(u8),
    ReclaimIdle,
    Quiesce,
    Recover,
}

/// Shorthand for an append followed by a completed write.
fn write(pages: u8) -> Op {
    Op::Append {
        pages,
        io: Io::Complete,
        fault: None,
    }
}

/// Shorthand for a quiesced restore.
fn restore(image: Image) -> Op {
    Op::Restore {
        image,
        quiesce: true,
    }
}

/// Shorthand for a reclaim with generous budgets.
fn reclaim_all() -> Op {
    Op::Reclaim {
        reserve: SLOTS as u8,
        visits: 2 * SLOTS as u8,
        entries: 16,
    }
}

/// Transitions reached across a set of runs; zero means a case did not test it.
#[derive(Clone, Copy, Debug, Default)]
struct Coverage {
    restores_ok: usize,
    restore_bumps: usize,
    restore_stale: usize,
    restore_corrupt: usize,
    restore_busy: usize,
    restore_unavailable: usize,
    reissues: usize,
    recycles: usize,
    partial_evictions: usize,
    write_failures: usize,
    read_failures: usize,
    abandoned: usize,
    recoveries: usize,
    data_checks: usize,
    publishes: usize,
}

impl Coverage {
    /// Accumulate counters from another run.
    fn add(&mut self, other: Self) {
        self.restores_ok += other.restores_ok;
        self.publishes += other.publishes;
        self.restore_bumps += other.restore_bumps;
        self.restore_stale += other.restore_stale;
        self.restore_corrupt += other.restore_corrupt;
        self.restore_busy += other.restore_busy;
        self.restore_unavailable += other.restore_unavailable;
        self.reissues += other.reissues;
        self.recycles += other.recycles;
        self.partial_evictions += other.partial_evictions;
        self.write_failures += other.write_failures;
        self.read_failures += other.read_failures;
        self.abandoned += other.abandoned;
        self.recoveries += other.recoveries;
        self.data_checks += other.data_checks;
    }
}

/// Lifecycle of the bytes behind one appended extent.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Status {
    Reserved,

    Pending,

    Written,

    Failed,

    Unknown,
}

/// One allocation incarnation in the independent model.
#[derive(Clone, Debug)]
struct Record {
    seg: usize,

    generation: u64,

    extent: Extent,

    fill: u8,

    status: Status,

    superseded: bool,
}

/// Caller index of published mappings, drained oldest first by the clock.
#[derive(Default)]
struct Index {
    map: RefCell<BTreeMap<u64, VecDeque<usize>>>,

    unpublished: RefCell<[usize; SLOTS]>,

    removed: Cell<usize>,
}

impl Index {
    /// Total mappings across all segments.
    fn total(&self) -> usize {
        self.map.borrow().values().map(VecDeque::len).sum()
    }
}

impl SegmentEntries for Index {
    /// Unpublished writes veto starting eviction of a sealed segment.
    fn can_evict(&self, segment: SegmentId) -> bool {
        self.unpublished.borrow()[segment.0 as usize] == 0
    }

    /// Remove up to budget oldest mappings.
    fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize {
        let mut map = self.map.borrow_mut();
        let Some(queue) = map.get_mut(&segment.0) else {
            return 0;
        };
        let removed = budget.min(queue.len());
        queue.drain(..removed);
        self.removed.set(self.removed.get() + removed);
        removed
    }

    /// Whether any mapping still names the segment.
    fn is_empty(&self, segment: SegmentId) -> bool {
        self.map
            .borrow()
            .get(&segment.0)
            .is_none_or(VecDeque::is_empty)
    }
}

/// State that survives table recovery.
struct World {
    records: Vec<Record>,

    index: Index,

    history: Vec<Vec<SegmentSnapshot>>,

    max_generation: [u64; SLOTS],

    charge: Rc<Cell<usize>>,

    coverage: Coverage,

    last_restore: Option<Result<(), Error>>,

    fill: u8,
}

/// One table incarnation over the shared simulated file.
struct Table {
    slab: Slab<Counted>,

    segments: Rc<Segments>,

    clock: SegmentClock,

    reactor: Reactor<TestScope, ()>,
}

impl Table {
    /// Open the slab and an unconfigured table, checking simulated alignment.
    fn open() -> Self {
        let slab = Slab::new(PATH.into(), CAPACITY, SEGMENT, SEGMENT as usize);
        let segments = Rc::new(Segments::new(SEGMENT));
        let alignment = slab.open_configured(&segments).unwrap();
        assert_eq!(alignment.length() as u64, PAGE);
        assert_eq!(alignment.offset(), PAGE);
        Self {
            slab,
            clock: SegmentClock::new(segments.clone()),
            segments,
            reactor: Reactor::new(32, ()),
        }
    }
}

/// A read or write future that still owns its lease and buffer.
struct Pending<'t> {
    id: u64,

    record: usize,

    write: bool,

    fails: Option<bool>,

    polled: bool,

    operation: Operation<'t, AlignedBuffer<Counted>, TestError>,
}

thread_local! {
    static CONTEXT: RefCell<String> = const { RefCell::new(String::new()) };
}

/// Print the failing case, step, and operation when an assertion panics.
struct Context_;

impl Drop for Context_ {
    /// Report replay context only on failure.
    fn drop(&mut self) {
        if std::thread::panicking() {
            CONTEXT.with(|c| eprintln!("DST failure at {}", c.borrow()));
        }
    }
}

/// Record the replay context for the current step.
fn context(text: String) {
    CONTEXT.with(|c| *c.borrow_mut() = text);
}

/// Expected append result and table image under the documented append policy.
fn expect_append(
    pre: &[SegmentSnapshot],
    length: u64,
    frozen: bool,
) -> (Res<(usize, u64)>, Vec<SegmentSnapshot>) {
    let mut post = pre.to_vec();
    if frozen {
        return (Err(Error::Busy), post);
    }
    let open = pre.iter().position(|s| s.state == SegmentState::Open);
    let target = match open {
        Some(o) if pre[o].used_bytes + length <= SEGMENT => Some(o),
        _ => pre.iter().position(|s| s.state == SegmentState::Free),
    };
    if let Some(o) = open
        && target != Some(o)
    {
        post[o].state = SegmentState::Sealed;
    }
    let Some(t) = target else {
        return (Err(Error::Busy), post);
    };
    let offset = post[t].used_bytes;
    post[t].used_bytes += length;
    post[t].state = if post[t].used_bytes == SEGMENT {
        SegmentState::Sealed
    } else {
        SegmentState::Open
    };
    (Ok((t, offset)), post)
}

type Res<T> = std::result::Result<T, Error>;

/// Expected restore result and published image under the restore policy.
///
/// Busy outranks every image error. Structural damage anywhere in the image is
/// Corrupt before any generation is compared. A generation below the live slot
/// is Stale. Turning an occupied slot Free at its live generation publishes the
/// next generation, so bytes are never reissued under a generation that earlier
/// mappings carry; Unavailable when that generation would wrap.
fn expect_restore(
    live: &[SegmentSnapshot],
    image: &[SegmentSnapshot],
    busy: bool,
) -> Res<Vec<SegmentSnapshot>> {
    if busy {
        return Err(Error::Busy);
    }
    if image.len() != live.len() {
        return Err(Error::Corrupt);
    }
    let mut opens = 0;
    for (i, s) in image.iter().enumerate() {
        let free = s.state == SegmentState::Free;
        if s.id.0 != i as u64
            || s.generation.0 == 0
            || s.used_bytes > SEGMENT
            || s.used_bytes % PAGE != 0
            || free != (s.used_bytes == 0)
        {
            return Err(Error::Corrupt);
        }
        if s.state == SegmentState::Open {
            opens += 1;
            if s.used_bytes == SEGMENT {
                return Err(Error::Corrupt);
            }
        }
    }
    if opens > 1 {
        return Err(Error::Corrupt);
    }
    if image
        .iter()
        .zip(live)
        .any(|(s, l)| s.generation.0 < l.generation.0)
    {
        return Err(Error::Stale);
    }
    let mut out = image.to_vec();
    for (s, l) in out.iter_mut().zip(live) {
        if s.state == SegmentState::Open {
            s.state = SegmentState::Sealed;
        }
        if s.state == SegmentState::Free
            && s.generation == l.generation
            && l.state != SegmentState::Free
        {
            s.generation = Generation(l.generation.0.checked_add(1).ok_or(Error::Unavailable)?);
        }
    }
    Ok(out)
}

/// Expected validate result for one record against a live image.
fn expect_validate(record: &Record, live: &[SegmentSnapshot]) -> Res<()> {
    let slot = &live[record.seg];
    if slot.generation.0 != record.generation
        || !matches!(slot.state, SegmentState::Open | SegmentState::Sealed)
    {
        return Err(Error::Stale);
    }
    let start = record.seg as u64 * SEGMENT;
    if record.extent.offset() + record.extent.length() as u64 > start + slot.used_bytes {
        return Err(Error::Corrupt);
    }
    Ok(())
}

/// Expected lease result for one record against a live image.
fn expect_lease(record: &Record, live: &[SegmentSnapshot]) -> Res<()> {
    let slot = &live[record.seg];
    if slot.generation.0 != record.generation
        || !matches!(slot.state, SegmentState::Open | SegmentState::Sealed)
    {
        return Err(Error::Stale);
    }
    Ok(())
}

/// Pick from the newest end of a list.
fn newest(len: usize, pick: u8) -> Option<usize> {
    (len != 0).then(|| len - 1 - pick as usize % len)
}

/// Runner state for one table incarnation.
struct Phase<'t, 'w> {
    sim: &'w Simulation,

    t: &'t Table,

    w: &'w mut World,

    pending: Vec<Pending<'t>>,

    next_id: u64,

    leases: Vec<SegmentLease>,

    buffers: Vec<AlignedBuffer<Counted>>,

    frozen: Option<FreezeGuard>,

    /// Dropped futures that were submitted may still own leases.
    maybe_held: bool,
}

impl<'t, 'w> Phase<'t, 'w> {
    /// Start a phase with no caller-owned resources.
    fn new(sim: &'w Simulation, t: &'t Table, w: &'w mut World) -> Self {
        Self {
            sim,
            t,
            w,
            pending: Vec::new(),
            next_id: 0,
            leases: Vec::new(),
            buffers: Vec::new(),
            frozen: None,
            maybe_held: false,
        }
    }

    /// Live table image.
    fn snap(&self) -> Vec<SegmentSnapshot> {
        self.t.segments.snapshot()
    }

    /// Whether any lease certainly or possibly remains outstanding.
    fn busy(&self) -> bool {
        self.frozen.is_some()
            || !self.leases.is_empty()
            || !self.pending.is_empty()
            || self.t.reactor.in_flight() != 0
    }

    /// Whether a caller lease or live future certainly holds this segment.
    fn held(&self, seg: usize) -> bool {
        self.leases.iter().any(|l| l.id().0 as usize == seg)
            || self
                .pending
                .iter()
                .any(|p| self.w.records[p.record].seg == seg)
    }

    /// Run operations from start; return the index of a Recover if one ends the phase.
    fn run(&mut self, case: &str, ops: &[Op], start: usize) -> Option<usize> {
        for (step, op) in ops.iter().enumerate().skip(start) {
            context(format!("{case} step {step}: {op:?}"));
            if std::env::var_os("DST_VERBOSE").is_some() {
                eprintln!("{case} step {step}: {op:?} on {:?}", self.snap());
            }
            if matches!(op, Op::Recover) {
                return Some(step);
            }
            self.step(op);
            self.invariants();
        }
        None
    }

    /// Execute one operation and check its specific contract.
    fn step(&mut self, op: &Op) {
        match op {
            Op::Append { pages, io, fault } => self.append(*pages, *io, *fault),
            Op::Read {
                pick,
                fault,
                abandon,
            } => self.read(*pick, *fault, *abandon),
            Op::ReadAll => self.read_all(),
            Op::Turn(turns) => self.turn(*turns),
            Op::Abandon(pick) => self.abandon(*pick),
            Op::Lease(pick) => self.lease(*pick),
            Op::Release(pick) => {
                if let Some(i) = newest(self.leases.len(), *pick) {
                    drop(self.leases.remove(i));
                }
            }
            Op::Snapshot => {
                if self.w.history.len() == HISTORY {
                    self.w.history.remove(0);
                }
                let snap = self.snap();
                self.w.history.push(snap);
            }
            Op::Restore { image, quiesce } => self.restore(image, *quiesce),
            Op::ExpectRestore(expected) => assert_eq!(self.w.last_restore, Some(*expected)),
            Op::Freeze => {
                let result = self.t.segments.freeze();
                if self.frozen.is_some() {
                    assert_eq!(result.err(), Some(Error::Busy));
                } else {
                    self.frozen = Some(result.unwrap());
                }
            }
            Op::Thaw => self.frozen = None,
            Op::Reclaim {
                reserve,
                visits,
                entries,
            } => self.reclaim(*reserve, *visits, *entries, None),
            Op::Scored {
                reserve,
                visits,
                entries,
                salt,
            } => self.reclaim(*reserve, *visits, *entries, Some(*salt)),
            Op::ReclaimIndex { visits, keep } => self.reclaim_index(*visits, *keep),
            Op::MarkRead(slot) => {
                let id = SegmentId(u64::from(*slot) % SLOTS as u64);
                assert_eq!(self.t.clock.mark_read(id), Ok(()));
            }
            Op::Evict(slot) => self.evict(*slot as usize % SLOTS),
            Op::Recycle(slot) => self.recycle(*slot as usize % SLOTS),
            Op::Buffer(pages) => {
                if self.buffers.len() < MAX_BUFFERS {
                    let length = (u64::from(*pages % 2) + 1) as usize * PAGE as usize;
                    let buffer = self
                        .t
                        .slab
                        .allocate(length, Counted::new(&self.w.charge, length))
                        .unwrap();
                    assert!(buffer.as_slice().iter().all(|b| *b == 0));
                    self.buffers.push(buffer);
                }
            }
            Op::DropBuffer(pick) => {
                if let Some(i) = newest(self.buffers.len(), *pick) {
                    let mut buffer = self.buffers.remove(i);
                    buffer.as_mut_slice().fill(0xa5);
                }
            }
            Op::ReclaimIdle => {
                let idle = self.t.slab.idle_bytes();
                assert_eq!(self.t.slab.reclaim_idle(), idle);
                assert_eq!(self.t.slab.idle_bytes(), 0);
            }
            Op::Quiesce => self.quiesce(),
            Op::Recover => unreachable!("recover ends a phase"),
        }
    }

    /// Append and check placement, sealing, and the no-reissue invariant.
    fn append(&mut self, pages: u8, io: Io, fault: Option<FaultKind>) {
        let length = u64::from(pages.clamp(1, 2)) * PAGE;
        let pre = self.snap();
        let (expected, image) = expect_append(&pre, length, self.frozen.is_some());
        let result = self.t.segments.append(length as usize);
        assert_eq!(self.snap(), image, "append image");
        let (lease, extent) = match (result, expected) {
            (Err(error), Err(expected)) => {
                assert_eq!(error, expected);
                return;
            }
            (Ok(found), Ok(_)) => found,
            (result, expected) => panic!(
                "append {:?}, expected {expected:?}",
                result.map(|(_, extent)| extent)
            ),
        };
        let (seg, offset) = expected.unwrap();
        let generation = pre[seg].generation.0;
        assert_eq!(lease.id(), SegmentId(seg as u64));
        assert_eq!(lease.generation().0, generation);
        assert_eq!(extent.offset(), seg as u64 * SEGMENT + offset);
        assert_eq!(extent.length() as u64, length);
        let end = extent.offset() + length;
        for (i, record) in self.w.records.iter_mut().enumerate() {
            let start = record.extent.offset();
            if record.seg != seg
                || record.superseded
                || start >= end
                || start + record.extent.length() as u64 <= extent.offset()
            {
                continue;
            }
            // The core invariant: reissued bytes must carry a newer generation
            // than any mapping that named them, so old mappings cannot validate.
            assert!(
                record.generation < generation,
                "record {i} {record:?} reissued at generation {generation}"
            );
            record.superseded = true;
            self.w.coverage.reissues += 1;
        }
        self.w.fill = self.w.fill.wrapping_add(1).max(1);
        let record = self.w.records.len();
        self.w.records.push(Record {
            seg,
            generation,
            extent,
            fill: self.w.fill,
            status: Status::Reserved,
            superseded: false,
        });
        match io {
            Io::Lease => {
                if self.leases.len() < MAX_LEASES {
                    self.leases.push(lease);
                }
            }
            Io::Skip => drop(lease),
            Io::Complete | Io::Pending | Io::Abandon | Io::Unpolled => {
                let fault = if matches!(io, Io::Unpolled) {
                    None
                } else {
                    fault
                };
                let mut buffer = self
                    .t
                    .slab
                    .allocate(
                        length as usize,
                        Counted::new(&self.w.charge, length as usize),
                    )
                    .unwrap();
                assert!(buffer.as_slice().iter().all(|b| *b == 0));
                buffer.as_mut_slice().fill(self.w.fill);
                self.inject(true, fault);
                let operation = self
                    .t
                    .slab
                    .write(&self.t.reactor, extent, buffer, lease, &SCOPE);
                self.w.records[record].status = Status::Pending;
                self.w.index.unpublished.borrow_mut()[seg] += 1;
                let id = self.push(record, true, fault, operation);
                self.after_submit(id, io);
            }
        }
    }

    /// Queue a simulated fault for the next read or write.
    fn inject(&self, write: bool, fault: Option<FaultKind>) {
        let name = if write { "write" } else { "read" };
        match fault {
            None => {}
            Some(FaultKind::Errno) => self.sim.inject(name, Fault::Errno(libc_eio())).unwrap(),
            Some(FaultKind::Short) => self
                .sim
                .inject(name, Fault::Short(PAGE as usize / 2))
                .unwrap(),
            Some(FaultKind::Delay(n)) => self
                .sim
                .inject(name, Fault::Delay(usize::from(n % 8) + 1))
                .unwrap(),
            Some(FaultKind::Hold(n)) => self
                .sim
                .inject(name, Fault::HoldCompletion(usize::from(n % 8) + 1))
                .unwrap(),
            Some(FaultKind::Reject) => self.sim.reject_submissions(1).unwrap(),
        }
    }

    /// Track a new future.
    fn push(
        &mut self,
        record: usize,
        write: bool,
        fault: Option<FaultKind>,
        operation: Operation<'t, AlignedBuffer<Counted>, TestError>,
    ) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        self.pending.push(Pending {
            id,
            record,
            write,
            fails: fault.map_or(Some(false), FaultKind::fails),
            polled: false,
            operation,
        });
        id
    }

    /// Apply the requested polling discipline to a new future.
    fn after_submit(&mut self, id: u64, io: Io) {
        match io {
            Io::Complete => self.drive(id),
            Io::Pending | Io::Abandon => {
                let done = self.poll_id(id);
                if io == Io::Abandon && !done {
                    let i = self.find(id).unwrap();
                    self.drop_pending(i);
                }
            }
            _ => {}
        }
    }

    /// Position of a live future.
    fn find(&self, id: u64) -> Option<usize> {
        self.pending.iter().position(|p| p.id == id)
    }

    /// Poll a future once; true when it completed and was retired.
    fn poll_id(&mut self, id: u64) -> bool {
        let Some(i) = self.find(id) else {
            return true;
        };
        let pending = &mut self.pending[i];
        pending.polled = true;
        let ready = match pending
            .operation
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
        {
            Poll::Ready(result) => result,
            Poll::Pending => return false,
        };
        let pending = self.pending.remove(i);
        self.complete(&pending, ready);
        true
    }

    /// Drive one future to completion within a fixed turn budget.
    fn drive(&mut self, id: u64) {
        for _ in 0..TURNS {
            if self.poll_id(id) {
                return;
            }
            self.t.reactor.poll_budgeted(64).unwrap();
        }
        panic!("operation {id} did not complete in {TURNS} turns");
    }

    /// Drop a live future, transferring any submitted work to the reactor.
    fn drop_pending(&mut self, i: usize) {
        let pending = self.pending.remove(i);
        if pending.write {
            self.w.records[pending.record].status = Status::Unknown;
            self.w.index.unpublished.borrow_mut()[self.w.records[pending.record].seg] -= 1;
        }
        if pending.polled {
            self.maybe_held = true;
        }
        self.w.coverage.abandoned += 1;
    }

    /// Check a completion against its fault and publish successful writes.
    fn complete(
        &mut self,
        pending: &Pending<'t>,
        result: std::result::Result<AlignedBuffer<Counted>, TestError>,
    ) {
        let record = pending.record;
        let seg = self.w.records[record].seg;
        if pending.write {
            self.w.index.unpublished.borrow_mut()[seg] -= 1;
        }
        match result {
            Ok(buffer) => {
                assert_ne!(pending.fails, Some(true), "faulted I/O succeeded");
                if pending.write {
                    drop(buffer);
                    let r = &self.w.records[record];
                    assert!(!r.superseded, "write completed into reissued bytes");
                    // The completion owned the lease, so the bytes were never
                    // reissued, but eviction may have revoked read authority.
                    let expected = expect_validate(r, &self.snap());
                    assert_eq!(
                        self.t.segments.validate(
                            SegmentId(seg as u64),
                            Generation(r.generation),
                            &r.extent
                        ),
                        expected
                    );
                    self.w.records[record].status = Status::Written;
                    if expected.is_ok() {
                        self.w
                            .index
                            .map
                            .borrow_mut()
                            .entry(seg as u64)
                            .or_default()
                            .push_back(record);
                        self.w.coverage.publishes += 1;
                    }
                } else {
                    let fill = self.w.records[record].fill;
                    assert!(
                        buffer.as_slice().iter().all(|b| *b == fill),
                        "record {record} read back foreign bytes"
                    );
                    self.w.coverage.data_checks += 1;
                }
            }
            Err(error) => {
                assert_ne!(
                    pending.fails,
                    Some(false),
                    "unfaulted I/O failed: {error:?}"
                );
                if pending.write {
                    self.w.records[record].status = Status::Failed;
                    self.w.coverage.write_failures += 1;
                } else {
                    self.w.coverage.read_failures += 1;
                }
            }
        }
    }

    /// Records that are written, current, and readable now.
    fn readable(&self) -> Vec<usize> {
        let live = self.snap();
        (0..self.w.records.len())
            .filter(|i| {
                let r = &self.w.records[*i];
                r.status == Status::Written && !r.superseded && expect_validate(r, &live).is_ok()
            })
            .collect()
    }

    /// Read one readable record, optionally abandoning it after submission.
    fn read(&mut self, pick: u8, fault: Option<FaultKind>, abandon: bool) {
        let readable = self.readable();
        let Some(i) = newest(readable.len(), pick) else {
            return;
        };
        let id = self.submit_read(readable[i], fault);
        if abandon {
            if !self.poll_id(id) {
                let i = self.find(id).unwrap();
                self.drop_pending(i);
            }
        } else {
            self.drive(id);
        }
    }

    /// Lease and submit one read.
    fn submit_read(&mut self, record: usize, fault: Option<FaultKind>) -> u64 {
        let r = self.w.records[record].clone();
        let lease = self
            .t
            .segments
            .lease(SegmentId(r.seg as u64), Generation(r.generation))
            .unwrap();
        let length = r.extent.length();
        let buffer = self
            .t
            .slab
            .allocate(length, Counted::new(&self.w.charge, length))
            .unwrap();
        assert!(buffer.as_slice().iter().all(|b| *b == 0));
        self.inject(false, fault);
        let operation = self
            .t
            .slab
            .read(&self.t.reactor, r.extent, buffer, lease, &SCOPE);
        self.push(record, false, fault, operation)
    }

    /// Read back every readable record and compare its bytes.
    fn read_all(&mut self) {
        for record in self.readable() {
            let id = self.submit_read(record, None);
            self.drive(id);
        }
    }

    /// Advance the reactor and poll already-submitted futures.
    fn turn(&mut self, turns: u8) {
        for _ in 0..=turns % 8 {
            self.t.reactor.poll_budgeted(64).unwrap();
            let polled: Vec<u64> = self
                .pending
                .iter()
                .filter(|p| p.polled)
                .map(|p| p.id)
                .collect();
            for id in polled {
                self.poll_id(id);
            }
        }
    }

    /// Drop one live future.
    fn abandon(&mut self, pick: u8) {
        if let Some(i) = newest(self.pending.len(), pick) {
            self.drop_pending(i);
        }
    }

    /// Request a lease for any record and check it against the lease spec.
    fn lease(&mut self, pick: u8) {
        let Some(i) = newest(self.w.records.len(), pick) else {
            return;
        };
        let r = &self.w.records[i];
        let expected = expect_lease(r, &self.snap());
        let result = self
            .t
            .segments
            .lease(SegmentId(r.seg as u64), Generation(r.generation));
        assert_eq!(result.as_ref().map(|_| ()).err().copied(), expected.err());
        if let Ok(lease) = result
            && self.leases.len() < MAX_LEASES
        {
            self.leases.push(lease);
        }
    }

    /// Build a restore image relative to the live table.
    fn image(&self, image: &Image, live: &[SegmentSnapshot]) -> Vec<SegmentSnapshot> {
        let history = |pick: u8| {
            newest(self.w.history.len(), pick)
                .map_or_else(|| live.to_vec(), |i| self.w.history[i].clone())
        };
        match image {
            Image::History(pick) => history(*pick),
            Image::Current => live.to_vec(),
            Image::Truncated => live[..SLOTS - 1].to_vec(),
            Image::Edited { base, edits } => {
                let mut out = base.map_or_else(|| live.to_vec(), history);
                for edit in edits {
                    let slot = edit.slot as usize % SLOTS;
                    let s = &mut out[slot];
                    let g = live[slot].generation.0;
                    if let Some(state) = edit.state {
                        s.state = state;
                    }
                    if let Some(generation) = edit.generation {
                        s.generation = Generation(match generation {
                            Gen::Zero => 0,
                            Gen::Lower => g - 1,
                            Gen::Equal => g,
                            Gen::Higher => g.saturating_add(1),
                            Gen::NearMax => u64::MAX - 1,
                            Gen::Max => u64::MAX,
                        });
                    }
                    if let Some(used) = edit.used {
                        s.used_bytes = used;
                    }
                    if edit.wrong_id {
                        s.id = SegmentId((slot as u64 + 1) % SLOTS as u64);
                    }
                }
                out
            }
        }
    }

    /// Validate and restore an image, checking the policy and atomic failure.
    fn restore(&mut self, image: &Image, quiesce: bool) {
        if quiesce {
            self.quiesce();
        }
        let live = self.snap();
        let free = self.t.segments.free_count();
        let image = self.image(image, &live);
        let expected = expect_restore(&live, &image, self.busy());
        let probe = self.t.segments.validate_restore(&image);
        assert_eq!(probe, expected.as_ref().map(|_| ()).map_err(|e| *e));
        assert_eq!(self.snap(), live, "validate_restore mutated the table");
        let result = self.t.segments.restore(image);
        assert_eq!(result, expected.as_ref().map(|_| ()).map_err(|e| *e));
        self.w.last_restore = Some(result);
        let now = self.snap();
        let c = &mut self.w.coverage;
        match expected {
            Ok(image) => {
                assert_eq!(now, image);
                c.restores_ok += 1;
                c.restore_bumps += image
                    .iter()
                    .zip(&live)
                    .filter(|(s, l)| {
                        s.state == SegmentState::Free
                            && l.generation.0.checked_add(1) == Some(s.generation.0)
                            && l.state != SegmentState::Free
                    })
                    .count();
            }
            Err(error) => {
                assert_eq!(now, live, "failed restore published state");
                assert_eq!(self.t.segments.free_count(), free);
                match error {
                    Error::Busy => c.restore_busy += 1,
                    Error::Corrupt => c.restore_corrupt += 1,
                    Error::Stale => c.restore_stale += 1,
                    Error::Unavailable => c.restore_unavailable += 1,
                    _ => unreachable!(),
                }
            }
        }
    }

    /// Run a physical reclaim and check its bounded lifecycle transitions.
    fn reclaim(&mut self, reserve: u8, visits: u8, entries: u8, salt: Option<u8>) {
        let (reserve, visits, entries) = (
            usize::from(reserve) % (SLOTS + 1),
            usize::from(visits) % (2 * SLOTS + 2),
            usize::from(entries) % 6,
        );
        let pre = self.snap();
        let unpublished = *self.w.index.unpublished.borrow();
        let removed = self.w.index.removed.get();
        let result = match salt {
            None => self
                .t
                .clock
                .reclaim(&self.w.index, reserve, visits, entries),
            Some(salt) => {
                self.t
                    .clock
                    .reclaim_scored(&self.w.index, reserve, visits, entries, |id| {
                        (id.0 + 1).wrapping_mul(u64::from(salt)) % 5
                    })
            }
        };
        let post = self.snap();
        let removed = self.w.index.removed.get() - removed;
        assert!(removed <= entries, "removed {removed} > {entries}");
        assert!(
            matches!(result, Ok(()) | Err(Error::Busy | Error::Unavailable)),
            "{result:?}"
        );
        if reserve == 0 || self.frozen.is_some() {
            assert_eq!(post, pre);
            if reserve == 0 {
                assert_eq!(result, Ok(()));
                assert_eq!(removed, 0);
            }
        }
        if result.is_ok() && reserve != 0 {
            assert!(self.t.segments.free_count() >= reserve.min(SLOTS));
            assert!(post.iter().all(|s| s.state != SegmentState::Evicting));
        }
        self.check_reclaim(&pre, &post, unpublished);
    }

    /// Allowed reclaim transitions, independent of the scheduling policy.
    fn check_reclaim(
        &mut self,
        pre: &[SegmentSnapshot],
        post: &[SegmentSnapshot],
        unpublished: [usize; SLOTS],
    ) {
        for (seg, (a, b)) in pre.iter().zip(post).enumerate() {
            if a == b {
                if b.state == SegmentState::Evicting && !self.w.index.is_empty(b.id) {
                    self.w.coverage.partial_evictions += 1;
                }
                continue;
            }
            match (a.state, b.state) {
                (SegmentState::Sealed, SegmentState::Evicting) => {
                    assert_eq!((a.generation, a.used_bytes), (b.generation, b.used_bytes));
                    assert_eq!(unpublished[seg], 0, "evicted past the writer veto");
                    if !self.w.index.is_empty(b.id) {
                        self.w.coverage.partial_evictions += 1;
                    }
                }
                (SegmentState::Sealed | SegmentState::Evicting, SegmentState::Free) => {
                    if a.state == SegmentState::Sealed {
                        assert_eq!(unpublished[seg], 0, "evicted past the writer veto");
                    }
                    self.check_recycled(seg, a, b);
                }
                _ => panic!("reclaim changed slot {seg}: {a:?} -> {b:?}"),
            }
        }
    }

    /// A recycled slot advanced its generation with no mapping or lease left.
    fn check_recycled(&mut self, seg: usize, a: &SegmentSnapshot, b: &SegmentSnapshot) {
        assert_eq!(b.generation.0, a.generation.0 + 1);
        assert_eq!(b.used_bytes, 0);
        assert!(self.w.index.is_empty(b.id), "recycled with live mappings");
        assert!(!self.held(seg), "recycled under a held lease");
        self.w.coverage.recycles += 1;
    }

    /// Drop index mappings without touching the table.
    fn reclaim_index(&mut self, visits: u8, keep: u8) {
        let (visits, keep) = (usize::from(visits) % (2 * SLOTS + 2), usize::from(keep) % 4);
        let pre = self.snap();
        let removed = self.w.index.removed.get();
        let index = &self.w.index;
        let result = self
            .t
            .clock
            .reclaim_index(index, visits, || index.total() <= keep);
        let removed = self.w.index.removed.get() - removed;
        assert_eq!(self.snap(), pre);
        assert!(removed <= visits.min(2 * SLOTS));
        assert_eq!(result.is_ok(), self.w.index.total() <= keep, "{result:?}");
    }

    /// Enter eviction directly.
    fn evict(&mut self, seg: usize) {
        let pre = self.snap();
        let result = self.t.segments.begin_evict(SegmentId(seg as u64));
        let mut post = pre.clone();
        let expected = if self.frozen.is_some()
            || !matches!(
                pre[seg].state,
                SegmentState::Sealed | SegmentState::Evicting
            ) {
            Err(Error::Busy)
        } else {
            post[seg].state = SegmentState::Evicting;
            Ok(())
        };
        assert_eq!(result, expected);
        assert_eq!(self.snap(), post);
    }

    /// Drain one evicting slot's mappings as its owner would, then recycle it.
    fn recycle(&mut self, seg: usize) {
        let pre = self.snap();
        if self.frozen.is_none() && pre[seg].state == SegmentState::Evicting {
            self.w
                .index
                .remove_bounded(SegmentId(seg as u64), usize::MAX);
        }
        let result = self.t.segments.recycle(SegmentId(seg as u64));
        let post = self.snap();
        if self.frozen.is_some() || pre[seg].state != SegmentState::Evicting || self.held(seg) {
            assert_eq!(result, Err(Error::Busy));
        } else {
            // Lease checks precede the generation bump, so a completion that
            // may still own a lease can turn either outcome into Busy.
            let expected = if pre[seg].generation.0 == u64::MAX {
                Err(Error::Unavailable)
            } else {
                Ok(())
            };
            if self.maybe_held && self.t.reactor.in_flight() != 0 {
                assert!(
                    result == expected || result == Err(Error::Busy),
                    "{result:?}"
                );
            } else {
                assert_eq!(result, expected);
            }
        }
        if result.is_ok() {
            self.check_recycled(seg, &pre[seg], &post[seg]);
            assert!(
                pre.iter()
                    .zip(&post)
                    .enumerate()
                    .all(|(i, (a, b))| i == seg || a == b)
            );
        } else {
            assert_eq!(post, pre);
        }
    }

    /// Release caller ownership, finish all I/O, and check accounting.
    fn quiesce(&mut self) {
        self.leases.clear();
        self.frozen = None;
        for _ in 0..TURNS {
            if self.pending.is_empty() && self.t.reactor.in_flight() == 0 {
                break;
            }
            let ids: Vec<u64> = self.pending.iter().map(|p| p.id).collect();
            for id in ids {
                self.poll_id(id);
            }
            self.t.reactor.poll_budgeted(64).unwrap();
        }
        assert!(self.pending.is_empty(), "I/O did not finish");
        assert_eq!(self.t.reactor.in_flight(), 0);
        let mut fence = self.t.slab.fence_writes();
        let mut done = false;
        for _ in 0..TURNS {
            if let Poll::Ready(result) =
                fence.as_mut().poll(&mut Context::from_waker(Waker::noop()))
            {
                result.unwrap();
                done = true;
                break;
            }
            self.t.reactor.poll_budgeted(64).unwrap();
        }
        assert!(done, "write fence did not finish");
        drop(fence);
        assert_eq!(self.t.slab.writes_in_flight(), 0);
        self.maybe_held = false;
        let live: usize = self.buffers.iter().map(AlignedBuffer::len).sum();
        assert_eq!(self.w.charge.get(), live + self.t.slab.idle_bytes());
    }

    /// Restore a recovery image into this fresh table.
    fn recover(&mut self, image: Vec<SegmentSnapshot>) {
        let live = self.snap();
        assert!(
            live.iter()
                .all(|s| s.state == SegmentState::Free && s.generation.0 == 1)
        );
        let expected = expect_restore(&live, &image, false).unwrap();
        self.t.segments.restore(image).unwrap();
        assert_eq!(self.snap(), expected);
        self.w.coverage.recoveries += 1;
    }

    /// Table-wide invariants checked after every operation.
    fn invariants(&mut self) {
        let snap = self.snap();
        assert_eq!(snap.len(), SLOTS);
        assert!(
            snap.iter()
                .filter(|s| s.state == SegmentState::Open)
                .count()
                <= 1
        );
        assert_eq!(
            self.t.segments.free_count(),
            snap.iter()
                .filter(|s| s.state == SegmentState::Free)
                .count()
        );
        for (i, s) in snap.iter().enumerate() {
            assert_eq!(s.id, SegmentId(i as u64));
            assert!(s.used_bytes <= SEGMENT && s.used_bytes % PAGE == 0);
            assert_eq!(s.state == SegmentState::Free, s.used_bytes == 0);
            assert!(
                s.generation.0 >= self.w.max_generation[i],
                "slot {i} generation rolled back"
            );
            self.w.max_generation[i] = s.generation.0;
        }
        for (i, r) in self.w.records.iter().enumerate() {
            let id = SegmentId(r.seg as u64);
            let generation = Generation(r.generation);
            let validated = self.t.segments.validate(id, generation, &r.extent);
            assert_eq!(validated, expect_validate(r, &snap), "record {i} {r:?}");
            let leased = self.t.segments.lease(id, generation).map(|_| ());
            assert_eq!(leased, expect_lease(r, &snap), "record {i} {r:?}");
            if r.superseded {
                assert!(validated.is_err(), "superseded record {i} validates");
                assert!(leased.is_err(), "superseded record {i} leases");
            }
        }
        assert!(self.w.charge.get() >= self.t.slab.idle_bytes());
    }
}

/// EIO without depending on libc in tests.
fn libc_eio() -> i32 {
    5
}

/// Run one case through the shared runner and return its coverage.
fn run(case: &str, cancel_first: bool, ops: &[Op]) -> Coverage {
    let _context = Context_;
    let sim = Simulation::new();
    let _environment = sim.enter();
    sim.set_cancel_first(cancel_first);
    let mut world = World {
        records: Vec::new(),
        index: Index::default(),
        history: Vec::new(),
        max_generation: [1; SLOTS],
        charge: Rc::new(Cell::new(0)),
        coverage: Coverage::default(),
        last_restore: None,
        fill: 0,
    };
    let mut start = 0;
    let mut recovered = None;
    loop {
        let table = Table::open();
        let mut phase = Phase::new(&sim, &table, &mut world);
        if let Some(image) = recovered.take() {
            context(format!("{case} recovery before step {start}"));
            phase.recover(image);
            phase.invariants();
        }
        if let Some(step) = phase.run(case, ops, start) {
            phase.quiesce();
            phase.buffers.clear();
            recovered = Some(phase.snap());
            start = step + 1;
            continue;
        }
        context(format!("{case} final checks"));
        phase.quiesce();
        phase.read_all();
        phase.buffers.clear();
        phase.invariants();
        break;
    }
    assert_eq!(world.charge.get(), 0, "charge leaked across tables");
    world.coverage
}

/// Random image edit biased toward generation and occupancy boundaries.
fn random_edit(rng: &mut Rng) -> Edit {
    const STATES: [SegmentState; 4] = [
        SegmentState::Free,
        SegmentState::Open,
        SegmentState::Sealed,
        SegmentState::Evicting,
    ];
    const GENS: [Gen; 6] = [
        Gen::Zero,
        Gen::Lower,
        Gen::Equal,
        Gen::Higher,
        Gen::NearMax,
        Gen::Max,
    ];
    const USED: [u64; 5] = [0, PAGE, SEGMENT, PAGE / 2, SEGMENT + PAGE];
    let used = USED[rng.below(5) as usize];
    let state = STATES[rng.below(4) as usize];
    Edit {
        slot: rng.byte(SLOTS as u64),
        state: rng.chance(80).then_some(state),
        generation: rng.chance(80).then(|| {
            if rng.chance(60) {
                Gen::Equal
            } else {
                GENS[rng.below(6) as usize]
            }
        }),
        // Mostly keep occupancy consistent with the chosen state.
        used: if rng.chance(70) {
            Some(match state {
                SegmentState::Free => 0,
                _ => PAGE * (rng.below(2) + 1),
            })
        } else {
            rng.chance(50).then_some(used)
        },
        wrong_id: rng.chance(3),
    }
}

/// Random fault, mostly none.
fn random_fault(rng: &mut Rng) -> Option<FaultKind> {
    if !rng.chance(25) {
        return None;
    }
    Some(match rng.below(5) {
        0 => FaultKind::Errno,
        1 => FaultKind::Short,
        2 => FaultKind::Delay(rng.byte(8)),
        3 => FaultKind::Hold(rng.byte(8)),
        _ => FaultKind::Reject,
    })
}

/// The single generator: a seed fully determines cancellation order and operations.
fn generate(seed: u64, steps: usize) -> (bool, Vec<Op>) {
    let mut rng = Rng(seed);
    let cancel_first = rng.chance(50);
    let mut ops = Vec::with_capacity(steps);
    while ops.len() < steps {
        let roll = rng.below(100);
        let op = match roll {
            0..=24 => Op::Append {
                pages: rng.byte(2) + 1,
                io: match rng.below(20) {
                    0..=9 => Io::Complete,
                    10..=12 => Io::Pending,
                    13..=14 => Io::Abandon,
                    15 => Io::Unpolled,
                    16..=17 => Io::Lease,
                    _ => Io::Skip,
                },
                fault: random_fault(&mut rng),
            },
            25..=32 => Op::Snapshot,
            33..=44 => Op::Restore {
                image: match rng.below(20) {
                    0..=7 => Image::History(rng.byte(8)),
                    8..=9 => Image::Current,
                    10 => Image::Truncated,
                    _ => Image::Edited {
                        base: rng.chance(50).then(|| rng.byte(8)),
                        edits: (0..=rng.below(2)).map(|_| random_edit(&mut rng)).collect(),
                    },
                },
                quiesce: rng.chance(70),
            },
            45..=52 => Op::Reclaim {
                reserve: rng.byte(4),
                visits: rng.byte(8),
                entries: rng.byte(6),
            },
            53..=56 => Op::Scored {
                reserve: rng.byte(4),
                visits: rng.byte(8),
                entries: rng.byte(6),
                salt: rng.byte(256),
            },
            57..=59 => Op::ReclaimIndex {
                visits: rng.byte(8),
                keep: rng.byte(4),
            },
            60..=62 => Op::Evict(rng.byte(SLOTS as u64)),
            63..=65 => Op::Recycle(rng.byte(SLOTS as u64)),
            66..=69 => Op::Lease(rng.byte(8)),
            70..=72 => Op::Release(rng.byte(8)),
            73..=78 => Op::Read {
                pick: rng.byte(8),
                fault: random_fault(&mut rng),
                abandon: rng.chance(30),
            },
            79..=83 => Op::Turn(rng.byte(8)),
            84..=86 => Op::Abandon(rng.byte(8)),
            87..=88 => Op::Freeze,
            89..=91 => Op::Thaw,
            92..=93 => Op::MarkRead(rng.byte(SLOTS as u64)),
            94 => Op::Buffer(rng.byte(2)),
            95 => Op::DropBuffer(rng.byte(4)),
            96 => Op::ReclaimIdle,
            97..=98 => Op::Quiesce,
            _ => Op::Recover,
        };
        ops.push(op);
    }
    (cancel_first, ops)
}

/// Environment override for replay and budget tuning.
fn env(name: &str) -> Option<u64> {
    std::env::var(name).ok().map(|v| v.parse().unwrap())
}

/// Seeded random sequences through the shared runner, with coverage floors.
#[test]
fn seeded_sequences_preserve_allocator_invariants() {
    let steps = env("DST_STEPS").unwrap_or(400) as usize;
    let seeds: Vec<u64> = match env("DST_SEED") {
        Some(seed) => vec![seed],
        None => (0..env("DST_SEEDS").unwrap_or(256)).collect(),
    };
    let mut coverage = Coverage::default();
    for seed in &seeds {
        let (cancel_first, ops) = generate(*seed, steps);
        coverage.add(run(&format!("seed {seed}"), cancel_first, &ops));
    }
    if seeds.len() > 1 {
        let c = coverage;
        for (name, count) in [
            ("restores_ok", c.restores_ok),
            ("restore_bumps", c.restore_bumps),
            ("restore_stale", c.restore_stale),
            ("restore_corrupt", c.restore_corrupt),
            ("restore_busy", c.restore_busy),
            ("reissues", c.reissues),
            ("recycles", c.recycles),
            ("partial_evictions", c.partial_evictions),
            ("write_failures", c.write_failures),
            ("read_failures", c.read_failures),
            ("abandoned", c.abandoned),
            ("recoveries", c.recoveries),
            ("data_checks", c.data_checks),
            ("publishes", c.publishes),
        ] {
            assert!(count > 0, "seeded DST never reached {name}: {c:?}");
        }
        if std::env::var_os("DST_COVERAGE").is_some() {
            eprintln!("{c:?}");
        }
    }
}

/// Phil's restore regressions and other fixed histories, each run through the
/// same runner with both cancellation orders.
#[test]
fn regression_traces() {
    use SegmentState::{Free, Open, Sealed};
    let traces: Vec<(&str, Vec<Op>)> = vec![
        (
            "same-generation restore must not revive an open slot",
            vec![
                Op::Snapshot,
                write(1),
                restore(Image::History(0)),
                Op::ExpectRestore(Ok(())),
                write(1),
            ],
        ),
        (
            "same-generation restore must not revive an evicting slot",
            vec![
                Op::Snapshot,
                write(1),
                write(1),
                Op::Reclaim {
                    reserve: 3,
                    visits: 1,
                    entries: 1,
                },
                restore(Image::History(0)),
                Op::ExpectRestore(Ok(())),
                write(1),
                write(1),
            ],
        ),
        (
            "same-generation restore must not revive a sealed slot",
            vec![Op::Snapshot, write(2), restore(Image::History(0)), write(1)],
        ),
        (
            "lower-generation restore is stale",
            vec![
                write(2),
                Op::Snapshot,
                reclaim_all(),
                restore(Image::History(0)),
                Op::ExpectRestore(Err(Error::Stale)),
            ],
        ),
        (
            "restoring a pre-recovery image after recovery bumps reused slots",
            vec![
                Op::Snapshot,
                write(1),
                Op::Recover,
                restore(Image::History(0)),
                Op::ExpectRestore(Ok(())),
                write(1),
            ],
        ),
        (
            "replaying a bumped free image is stale and current images do not bump again",
            vec![
                Op::Snapshot,
                write(1),
                restore(Image::History(0)),
                Op::ExpectRestore(Ok(())),
                restore(Image::History(0)),
                Op::ExpectRestore(Err(Error::Stale)),
                restore(Image::Current),
                Op::ExpectRestore(Ok(())),
                write(1),
            ],
        ),
        (
            "required bump at maximum generation is unavailable",
            vec![
                restore(Image::Edited {
                    base: None,
                    edits: vec![Edit::slot(0, Sealed, Gen::Max, SEGMENT)],
                }),
                restore(Image::Edited {
                    base: None,
                    edits: vec![Edit::slot(0, Free, Gen::Equal, 0)],
                }),
                Op::ExpectRestore(Err(Error::Unavailable)),
                write(2),
            ],
        ),
        (
            "required bump below maximum publishes the maximum",
            vec![
                restore(Image::Edited {
                    base: None,
                    edits: vec![Edit::slot(0, Open, Gen::NearMax, PAGE)],
                }),
                restore(Image::Edited {
                    base: None,
                    edits: vec![Edit::slot(0, Free, Gen::Equal, 0)],
                }),
                Op::ExpectRestore(Ok(())),
                write(1),
            ],
        ),
        (
            "late failing slot leaves an earlier bump unpublished",
            vec![
                write(1),
                restore(Image::Edited {
                    base: None,
                    edits: vec![
                        Edit::slot(0, Free, Gen::Equal, 0),
                        Edit {
                            wrong_id: true,
                            ..Edit::slot(2, Free, Gen::Equal, 0)
                        },
                    ],
                }),
                Op::ExpectRestore(Err(Error::Corrupt)),
                write(1),
            ],
        ),
        (
            "busy outranks corrupt and stale images",
            vec![
                write(1),
                Op::Snapshot,
                Op::Lease(0),
                Op::Restore {
                    image: Image::Truncated,
                    quiesce: false,
                },
                Op::ExpectRestore(Err(Error::Busy)),
                Op::Freeze,
                Op::Release(0),
                Op::Restore {
                    image: Image::History(0),
                    quiesce: false,
                },
                Op::ExpectRestore(Err(Error::Busy)),
            ],
        ),
        (
            "abandoned write keeps its lease until completion",
            vec![
                Op::Append {
                    pages: 2,
                    io: Io::Abandon,
                    fault: Some(FaultKind::Hold(7)),
                },
                Op::Evict(0),
                Op::Recycle(0),
                Op::Restore {
                    image: Image::Current,
                    quiesce: false,
                },
                Op::Quiesce,
                Op::Recycle(0),
                write(2),
            ],
        ),
    ];
    for (name, ops) in &traces {
        for cancel_first in [false, true] {
            run(name, cancel_first, ops);
        }
    }
}

/// Every live slot state against every restored slot state, generation,
/// occupancy, lease owner, and late failure, through the shared runner.
#[test]
fn restore_cross_product() {
    use SegmentState::{Evicting, Free, Open, Sealed};
    let prefix = vec![write(2), write(2), Op::Evict(0), Op::Recycle(0)];
    let live: [(&str, Vec<Op>); 5] = [
        ("free", vec![]),
        ("open", vec![write(1)]),
        ("sealed", vec![write(1), write(1)]),
        (
            "evicting",
            vec![
                write(1),
                write(1),
                Op::Reclaim {
                    reserve: 3,
                    visits: 1,
                    entries: 1,
                },
            ],
        ),
        (
            "sealed-max",
            vec![restore(Image::Edited {
                base: None,
                edits: vec![Edit::slot(0, Sealed, Gen::Max, SEGMENT)],
            })],
        ),
    ];
    let owners: [(&str, Vec<Op>); 3] = [
        ("unowned", vec![]),
        ("leased", vec![Op::Lease(0)]),
        (
            "completion",
            vec![Op::Read {
                pick: 0,
                fault: Some(FaultKind::Hold(7)),
                abandon: true,
            }],
        ),
    ];
    let mut coverage = Coverage::default();
    for (live_name, live_ops) in &live {
        for state in [Free, Open, Sealed, Evicting] {
            for generation in [
                Gen::Zero,
                Gen::Lower,
                Gen::Equal,
                Gen::Higher,
                Gen::NearMax,
                Gen::Max,
            ] {
                for used in [0, PAGE, SEGMENT, PAGE / 2, SEGMENT + PAGE] {
                    for (owner, owner_ops) in &owners {
                        for late in [false, true] {
                            let mut edits = vec![Edit::slot(0, state, generation, used)];
                            if late {
                                edits.push(Edit::slot(2, Free, Gen::Equal, PAGE));
                            }
                            let mut ops = prefix.clone();
                            ops.extend(live_ops.iter().cloned());
                            ops.extend(owner_ops.iter().cloned());
                            ops.push(Op::Restore {
                                image: Image::Edited { base: None, edits },
                                quiesce: false,
                            });
                            ops.extend([
                                Op::Quiesce,
                                write(1),
                                write(1),
                                write(1),
                                write(1),
                                Op::ReadAll,
                                reclaim_all(),
                                write(2),
                                write(2),
                            ]);
                            let case = format!(
                                "{live_name} -> {state:?}/{generation:?}/{used} {owner} late={late}"
                            );
                            coverage.add(run(&case, false, &ops));
                        }
                    }
                }
            }
        }
    }
    let c = coverage;
    assert!(c.restore_bumps > 0, "{c:?}");
    assert!(c.restore_unavailable > 0, "{c:?}");
    assert!(c.restore_stale > 0, "{c:?}");
    assert!(c.restore_busy > 0, "{c:?}");
    assert!(c.restore_corrupt > 0, "{c:?}");
    assert!(c.reissues > 0, "{c:?}");
}
