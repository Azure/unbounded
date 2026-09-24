// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local B+tree/bitmap slab cache.
//!
//! Create or open a [`Slab`] before starting workers, move each uniquely issued
//! [`SlabShard`] into its worker, then call [`Allocator::open`] there. All lookups use
//! memory. Call [`Allocator::poll`] from the application's completion loop; it
//! batches mutations into CoW checkpoints while new admissions continue.
//!
//! ```no_run
//! use racer_dataplane::{allocator::{Allocator, Config, Slab}, workers::WorkerContext};
//! # fn setup(context: &WorkerContext) -> std::io::Result<()> {
//! let mut slab = Slab::open("cache.slab", 32)?;
//! let shard = slab.take_shard(context.shard_ids()[0])?;
//! // Move `shard` into the pinned worker factory before opening it there.
//! let cache = Allocator::open(context, shard, Config::default())?;
//! assert!(cache.is_idle());
//! # Ok(()) }
//! ```
//!
//! Two alternating magic pages retain two recoverable roots. Data and tree writes
//! are synced before publishing a root, and the root is synced before releasing
//! its predecessor. Allocation ownership spans live entries, both checkpoints,
//! and kernel requests. Dropping the allocator may lose writes but cannot unlock
//! or recycle storage still accessed by the kernel.
//!
//! Formatting is explicit. Opening performs blocking metadata I/O at worker
//! setup only. Values use registered buffers; metadata writes use aligned pages.
//! The slab must reside on storage honoring fdatasync and independent aligned
//! page writes. The process holds an exclusive advisory lock for its lifetime.
//!
//! RACERS06 stores Content-Type object records and 64 MiB payload extents.
//! Leaves encode a 32-byte key, an 8-byte tag, then a 306-byte metadata record or
//! a payload page/length/CRC descriptor with zero padding (346 bytes total).
//! Leaf-page CRCs protect metadata; payload CRCs remain attached to extents.
//! The whole aligned eighth of each shard is reserved for index pages. Admission
//! budgets three complete trees/bitmaps (two roots plus a checkpoint in flight),
//! reserves entries for every payload extent, and caps metadata at a 16 MiB
//! resident allowance of 2 KiB per entry across CoW versions and LFU bookkeeping.
//! Metadata has no allocation class, value write, read lease, or buffer ownership.

use crate::buffers::{BUFFER_SIZE, Buffer, Fill};
use crate::metadata::{Metadata, ResidentMetadata};
use crate::uring::{self, BufferRange, FileOffset, Ring, Work};
use crate::workers::ShardId;
use std::cell::{Cell, RefCell};
use std::collections::{HashMap, VecDeque};
use std::ffi::CString;
use std::fs::{File, OpenOptions};
use std::io;
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{FileExt, OpenOptionsExt};
use std::path::Path;
use std::rc::{Rc, Weak};
use std::sync::Arc;

mod checkpoint;
mod disk;
mod eviction;
mod file;
mod layout;
mod placement;
mod slab;
mod space;
mod tree;

use checkpoint::{IoTicket, Job, Pipeline, RingIo};
pub use disk::crc64;
use disk::{
    Checkpoint, Intern, encode, get, magic, put, read_page, recover, seal, valid, validate_info,
};
pub use layout::{
    LayoutPlan, MAX_CAPACITY, MAX_PLANNED_SHARDS, MIN_CAPACITY, ResourceEstimate, TARGET_SHARD_SIZE,
};
#[cfg(test)]
use placement::ATTRIBUTE;
use placement::Layout;
#[cfg(test)]
use slab::{CreateStep, lock};

// Physical admission is advisory; quarantine is the safety boundary after IO ambiguity.
use std::sync::Mutex;

const HEADROOM: u64 = 64 * 1024 * 1024;
const RESIDENT_METADATA_BUDGET: usize = 16 * 1024 * 1024;

/// Shared by every worker/shard of one slab. Outstanding admissions remain
/// charged through durable checkpoint completion, not merely buffered WRITE.
#[derive(Default)]
struct Admission(Mutex<u64>, Mutex<CheckpointBudget>);

/// Share between active, prepared and retiring slabs to bound process-wide
/// checkpoint preparation. This is an internal concurrency bound, not a memory
/// budget. Two permits allow progress on a second shard during a slow fsync.
#[derive(Clone, Default)]
pub struct CheckpointBudget(Arc<std::sync::atomic::AtomicUsize>);
impl CheckpointBudget {
    pub const CONCURRENT: usize = 2;
    fn acquire(&self) -> Option<CheckpointPermit> {
        use std::sync::atomic::Ordering;
        self.0
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < Self::CONCURRENT).then_some(n + 1)
            })
            .ok()
            .map(|_| CheckpointPermit(self.clone()))
    }
}
struct CheckpointPermit(CheckpointBudget);
impl Drop for CheckpointPermit {
    fn drop(&mut self) {
        self.0.0.fetch_sub(1, std::sync::atomic::Ordering::AcqRel);
    }
}

impl Allocator {
    fn reserve_capacity(&mut self, bytes: u64) -> io::Result<()> {
        let mut pending = self.pressure.0.lock().unwrap();
        // Reserve the entire index class across ALL shards for checkpoint CoW,
        // plus filesystem margin. f_bavail excludes privileged reserved blocks.
        // Count pending bytes again even if ext4 has already charged them: this
        // deliberately errs toward early rejection rather than oversubscription.
        let geometry = self.space.geometry;
        let headroom = (geometry.range(Class::Index).1 as u64)
            .saturating_mul(PAGE_SIZE as u64)
            .saturating_mul(geometry.count)
            .saturating_add(HEADROOM);
        let required = headroom.saturating_add(*pending).saturating_add(bytes);
        let admission = check_disk_headroom(self.space._file.available_bytes(), required);
        // Cache retries intentionally replace the underlying error with a
        // bounded retry-limit error. Preserve the original evidence in tests.
        #[cfg(test)]
        if let Err(error) = &admission {
            eprintln!("{error}");
        }
        admission?;
        *pending += bytes;
        self.charged += bytes;
        Ok(())
    }

    fn release_capacity(&mut self, bytes: u64) {
        *self.pressure.0.lock().unwrap() -= bytes;
        self.charged -= bytes;
    }

    /// Poison is terminal until process restart and ordinary checkpoint recovery.
    pub fn is_failed(&self) -> bool {
        self.failed
    }

    /// Cache maintenance contains a poisoned allocator, never an arbitrary ring
    /// error. Do not call progress again, publish an ambiguous value, or reuse
    /// its allocator. Dropped tickets request cancellation; the ring still owns
    /// buffers, pages, descriptors and allocation pins through terminal CQEs.
    pub(crate) fn poll_contained(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        if self.failed {
            self.publish_diagnostics(ring.metrics());
            return Ok(Work::default());
        }
        match self.poll(ring, budget) {
            Err(error) if self.failed => {
                eprintln!(
                    "slab shard {} quarantined: {error}; restart after restoring storage capacity",
                    self.space.geometry.shard
                );
                self.pipeline = None;
                self.checkpoint_permit = None;
                self.pending.clear();
                self.publishing.clear();
                for read in self.reads.drain(..) {
                    *read.state.borrow_mut() =
                        Some(Err(io::Error::other("slab shard quarantined")));
                }
                // Failed/unsubmitted values must not monopolize shared NUMA
                // slots. Kernel WRITE owns its own Buffer; dropping these
                // application clones cannot release outstanding DMA storage.
                self.root.visit(&mut |_, value| {
                    if let Entry::Payload(value) = value {
                        value.buffer.borrow_mut().take();
                    }
                });
                self.publish_diagnostics(ring.metrics());
                Ok(Work {
                    runnable: true,
                    deadline: None,
                })
            }
            result => result,
        }
    }
}

pub const PAGE_SIZE: usize = 4096;
pub const DEFAULT_SLAB_SIZE: u64 = 10 * 1024 * 1024 * 1024;
// Daemon placement contract, attached to the slab inode before atomic publication.
// No in-place adoption/update: absence is ambiguous even for an empty legacy slab.

const WIDE: u64 = BUFFER_SIZE as u64;
const PAYLOAD_PAGES: usize = BUFFER_SIZE / PAGE_SIZE;
const FANOUT: usize = 11;
// Key (32), discriminant (8), inline metadata (306) or padded payload descriptor.
const LEAF_ENTRY: usize = 40 + Metadata::SIZE;
const MAGIC: u64 = u64::from_le_bytes(*b"RACERS06");
const NODE: u64 = u64::from_le_bytes(*b"RACERN06");
const BITS: u64 = u64::from_le_bytes(*b"RACERB01");
const BIT_BYTES: usize = PAGE_SIZE - 32;
const ROOT_HEADER_BYTES: usize = 72;
const ROOT_BITMAP_SLOTS: usize = (PAGE_SIZE - ROOT_HEADER_BYTES) / 8;
// One bit per slab page, rounded down to the existing whole-WIDE shard layout.
const MAX_SHARD_SIZE: u64 =
    ROOT_BITMAP_SLOTS as u64 * BIT_BYTES as u64 * 8 * PAGE_SIZE as u64 / WIDE * WIDE;
// Pressure reclaims at most a quarter of a class, and never more than one
// admission window. Small shards still reclaim at least one extent.
const RECLAIM_BATCH: usize = 64;
// Synchronous setup/recovery and asynchronous allocator operations refer to the
// same storage object.
pub(crate) enum SlabFile {
    Os(File, crate::slab_io::Io),
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn reject_version(page: &uring::Page) -> io::Result<()> {
    if page.0[..6] == *b"RACERS" && get(page, 0) != MAGIC {
        return Err(invalid(
            "incompatible slab format: expected RACERS06 with Content-Type metadata and 64 MiB payloads; preserve the old slab and use a new RACER_SLAB_PATH; no automatic migration/reformat",
        ));
    }
    Ok(())
}
fn busy() -> io::Error {
    io::ErrorKind::WouldBlock.into()
}

fn check_disk_headroom(available: io::Result<u64>, required: u64) -> io::Result<()> {
    match available {
        Ok(available) if available >= required => Ok(()),
        Ok(available) => Err(io::Error::new(
            io::ErrorKind::WouldBlock,
            format!(
                "slab filesystem admission: available={available} bytes, required={required} bytes including index reserve, filesystem margin, and pending/new writes"
            ),
        )),
        Err(error) => Err(io::Error::new(
            io::ErrorKind::WouldBlock,
            format!("slab filesystem admission: cannot query available bytes: {error}"),
        )),
    }
}

/// Digest of namespace, object, version and page identity, matching the buffer
/// pool's complete-identity convention. Metadata and payload keys must differ.
pub type Key = [u8; 32];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Kind {
    Metadata,
    Payload,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ValueInfo {
    pub kind: Kind,
    pub len: usize,
    pub crc64: u64,
    /// Always zero: payload extents are immutable and have no TTL.
    pub expires: u64,
}

#[derive(Clone, Copy, Debug)]
pub struct Config {
    pub max_pending_values: usize,
    pub max_io: usize,
    pub eviction_samples: usize,
    /// Frequency counters halve lazily after this many recorded hits.
    pub aging_interval: u64,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            max_pending_values: 64,
            max_io: 32,
            eviction_samples: 16,
            aging_interval: 65536,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct Geometry {
    base: u64,
    len: u64,
    shard: u64,
    count: u64,
}
impl Geometry {
    fn new(size: u64, count: usize, shard: usize) -> io::Result<Self> {
        if count == 0 || shard >= count {
            return Err(invalid(
                "slab shard count must be nonzero and shard ID must be below the count",
            ));
        }
        if size > i64::MAX as u64 || !size.is_multiple_of(WIDE) {
            return Err(invalid(
                "slab size must be a multiple of 64 MiB and at most i64::MAX bytes",
            ));
        }
        let len = size / count as u64 / WIDE * WIDE;
        if len < 8 * WIDE {
            return Err(invalid(
                "each shard needs at least 512 MiB; increase slab size or reduce shard count for a new slab",
            ));
        }
        // Check in u64 before any page counts become usize or allocate memory.
        // Empty initial roots omit bitmaps, but every future checkpoint must fit.
        let bitmap_pages = (len / PAGE_SIZE as u64)
            .div_ceil(8)
            .div_ceil(BIT_BYTES as u64);
        if bitmap_pages > ROOT_BITMAP_SLOTS as u64 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "slab geometry: {size} bytes across {count} shards gives {len} bytes/shard requiring {bitmap_pages} bitmap pages; RACERS06 roots hold at most {ROOT_BITMAP_SLOTS}, limiting each shard to {MAX_SHARD_SIZE} bytes (64 MiB aligned). Reduce slab size or increase RACER_SHARDS for a new slab; preserve an existing slab and use a new RACER_SLAB_PATH rather than changing its layout"
                ),
            ));
        }
        Ok(Self {
            base: shard as u64 * len,
            len,
            shard: shard as u64,
            count: count as u64,
        })
    }
    fn pages(self) -> usize {
        (self.len / PAGE_SIZE as u64) as usize
    }
    fn index_end(self) -> usize {
        // The entire aligned eighth is index CoW space. Metadata has no extent.
        ((self.len / 8 / WIDE).max(1) * WIDE / PAGE_SIZE as u64) as usize
    }
    fn metadata_limit(self) -> usize {
        let bitmaps = self.pages().div_ceil(8).div_ceil(BIT_BYTES);
        // At most 2N+1 nodes per tree (no unary non-root branches), plus
        // bitmaps, for BOTH retained roots and one independent frozen root.
        let entries = ((self.index_end() - 2) / 3 - bitmaps - 1) / 2;
        // Budget 2 KiB/entry for four resident tree versions, Vec spare
        // capacity, Rc headers, heat and hash-table capacity. This is a
        // conservative accounting allowance, not just sizeof(Metadata).
        let resident_entries = (self.index_end() * PAGE_SIZE).min(RESIDENT_METADATA_BUDGET) / 2048;
        entries
            .saturating_sub(self.range(Class::Payload).1)
            .min(resident_entries)
    }
    fn range(self, class: Class) -> (usize, usize, usize) {
        let end = self.index_end();
        match class {
            Class::Index => (2, end - 2, 1),
            Class::Payload => (end, (self.pages() - end) / PAYLOAD_PAGES, PAYLOAD_PAGES),
        }
    }
    fn offset(self, page: usize) -> u64 {
        self.base + page as u64 * PAGE_SIZE as u64
    }
}

/// Exclusive slab owner. Opening never creates, truncates, or reformats a file.
pub struct Slab {
    file: Arc<SlabFile>,
    pressure: Arc<Admission>,
    size: u64,
    shards: Vec<bool>,
}

/// Unique, transferable setup capability. It becomes thread-local when opened.
///
/// ```compile_fail
/// use racer_dataplane::{allocator::{Allocator, Config, SlabShard}, workers::WorkerContext};
/// fn open_twice(context: &WorkerContext, shard: SlabShard) {
///     let _ = Allocator::open(context, shard, Config::default());
///     let _ = Allocator::open(context, shard, Config::default());
/// }
/// ```
pub struct SlabShard {
    pressure: Arc<Admission>,
    file: Arc<SlabFile>,
    geometry: Geometry,
}

/// Validated empty storage with all filesystem setup already completed. Affine
/// allocator state is constructed only on its owning worker.
pub(crate) struct EmptyShard {
    pub(crate) shard: SlabShard,
    descriptor: File,
}
impl SlabShard {
    pub(crate) fn file_identity(&self) -> Arc<SlabFile> {
        self.file.clone()
    }
    pub fn id(&self) -> ShardId {
        ShardId::at(self.geometry.shard as usize)
    }
    pub fn count(&self) -> usize {
        self.geometry.count as usize
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Class {
    Index,
    Payload,
}
impl Class {
    fn index(self) -> usize {
        match self {
            Self::Index => 0,
            Self::Payload => 1,
        }
    }
}

// Two-level bitmap: set bits are free, summary bits identify nonempty words.
struct Bitmap {
    words: Vec<u64>,
    summary: Vec<u64>,
    free: usize,
}
impl Bitmap {
    fn new(count: usize) -> Self {
        fn full_bits(count: usize) -> Vec<u64> {
            let mut words = vec![u64::MAX; count.div_ceil(64)];
            if !count.is_multiple_of(64) {
                *words.last_mut().unwrap() = (1 << (count % 64)) - 1;
            }
            words
        }
        Self {
            words: full_bits(count),
            summary: full_bits(count.div_ceil(64)),
            free: count,
        }
    }
    fn set(&mut self, i: usize, free: bool) {
        let word = i / 64;
        let bit = 1 << (i % 64);
        let was = self.words[word] & bit != 0;
        if free {
            self.words[word] |= bit;
        } else {
            self.words[word] &= !bit;
        }
        self.free = self.free + usize::from(free) - usize::from(was);
        let summary = &mut self.summary[word / 64];
        let mask = 1 << (word % 64);
        if self.words[word] == 0 {
            *summary &= !mask;
        } else {
            *summary |= mask;
        }
    }
    fn release(&mut self, i: usize) {
        self.set(i, true);
    }
    fn take(&mut self) -> Option<usize> {
        let (s, &bits) = self
            .summary
            .iter()
            .enumerate()
            .find(|(_, bits)| **bits != 0)?;
        let word = s * 64 + bits.trailing_zeros() as usize;
        let i = word * 64 + self.words[word].trailing_zeros() as usize;
        self.set(i, false);
        Some(i)
    }
}
struct Space {
    geometry: Geometry,
    maps: [RefCell<Bitmap>; 2],
    retired: RefCell<Vec<(Class, usize, std::sync::Weak<()>)>>,
    // Holding this descriptor keeps the slab's exclusive lock alive, including
    // inside ring-owned keepalives after application teardown.
    _file: Arc<SlabFile>,
}
struct Allocation {
    space: Rc<Space>,
    class: Class,
    index: usize,
    pin: Arc<()>,
}

struct PayloadExtent {
    allocation: Rc<Allocation>,
    info: ValueInfo,
    // Kept until the write completes. New lookups can use the immutable bytes.
    buffer: RefCell<Option<Buffer>>,
    written: Cell<bool>,
}

#[derive(Clone)]
enum Entry {
    Metadata(ResidentMetadata),
    Payload(Rc<PayloadExtent>),
}
impl Entry {
    fn kind(&self) -> Kind {
        match self {
            Self::Metadata(_) => Kind::Metadata,
            Self::Payload(_) => Kind::Payload,
        }
    }
    fn payload(&self) -> Option<&Rc<PayloadExtent>> {
        match self {
            Self::Payload(value) => Some(value),
            Self::Metadata(_) => None,
        }
    }
}

#[derive(Clone)]
enum Body {
    Leaf(Vec<(Key, Entry)>),
    Branch(Vec<Rc<Node>>),
}
#[derive(Clone)]
struct Node {
    body: Body,
    disk: Option<Rc<Allocation>>,
}

struct Heat {
    key: Key,
    count: u16,
    epoch: u64,
}

/// Pins one version across replacement and eviction. There are no public disk
/// offsets: submit the lease through its owning allocator.
///
/// ```compile_fail
/// use racer_dataplane::allocator::ReadLease;
/// fn send<T: Send>() {}
/// send::<ReadLease>();
/// ```
#[must_use]
pub struct ReadLease {
    value: Rc<PayloadExtent>,
}
impl ReadLease {
    pub fn ready(&self) -> Option<FileValue> {
        self.value.written.get().then(|| FileValue {
            file: self.value.allocation.space._file.clone(),
            _pin: self.value.allocation.pin.clone(),
            offset: self.value.allocation.offset(),
            info: self.value.info,
        })
    }
    pub fn info(&self) -> ValueInfo {
        self.value.info
    }
    pub fn buffer(&self) -> Option<Buffer> {
        self.value.buffer.borrow().clone()
    }
}

/// Immutable, shareable read capability. Its private allocation pin prevents
/// reuse even when the owning worker's tree and checkpoints have released it.
///
/// ```
/// use racer_dataplane::allocator::FileValue;
/// fn shared<T: Send + Sync>() {}
/// shared::<FileValue>();
/// ```
///
/// File offsets and writable descriptors cannot escape this capability:
/// ```compile_fail
/// use racer_dataplane::allocator::FileValue;
/// fn overwrite(value: FileValue) { let _ = value.descriptor(); }
/// ```
#[derive(Clone)]
pub struct FileValue {
    file: Arc<SlabFile>,
    _pin: Arc<()>,
    offset: u64,
    info: ValueInfo,
}

/// Worker-local descriptor cache for a streaming response. Retain the slab's
/// identity, not an extent pin, so completed chunks can be reclaimed. Keeping
/// only the current slab bounds descriptor usage even for multi-slab streams.
pub(crate) struct FileSource {
    slab: Arc<SlabFile>,
    descriptor: uring::File,
}
impl FileSource {
    pub(crate) fn new(value: &FileValue) -> io::Result<Self> {
        Ok(Self {
            slab: value.file.clone(),
            descriptor: value.descriptor()?,
        })
    }
    pub(crate) fn descriptor(&mut self, value: &FileValue) -> io::Result<uring::File> {
        if !Arc::ptr_eq(&self.slab, &value.file) {
            *self = Self::new(value)?;
        }
        Ok(self.descriptor.clone())
    }
}

impl FileValue {
    #[cfg(test)]
    pub(crate) fn weak_pin(&self) -> std::sync::Weak<()> {
        Arc::downgrade(&self._pin)
    }
    pub fn info(&self) -> ValueInfo {
        self.info
    }
    pub(crate) fn descriptor(&self) -> io::Result<uring::File> {
        self.file.descriptor()
    }
    pub(crate) fn offset(&self) -> u64 {
        self.offset
    }
    pub fn read(
        &self,
        ring: &mut Ring,
        fill: Fill,
    ) -> Result<uring::Ticket<uring::Read>, uring::Rejected<Fill>> {
        let file = match self.descriptor() {
            Ok(file) => file,
            Err(error) => {
                return Err(uring::Rejected {
                    error,
                    resource: fill,
                });
            }
        };
        let ticket = ring.read(
            file.into(),
            fill,
            BufferRange::new(0..self.info.len).unwrap(),
            FileOffset::new(self.offset).unwrap(),
        )?;
        ring.retain(&ticket, Rc::new(self.clone()));
        Ok(ticket)
    }
}

type ReadState = Rc<RefCell<Option<io::Result<Buffer>>>>;
/// Observation only. Dropping this handle does not release an in-flight extent.
#[must_use]
pub struct ReadHandle {
    state: ReadState,
}
impl ReadHandle {
    /// Returns a result once. Pending and already-collected handles return None.
    pub fn take(&mut self) -> Option<io::Result<Buffer>> {
        self.state.borrow_mut().take()
    }
}
struct Reading {
    ticket: uring::Ticket<uring::Read>,
    state: ReadState,
    value: Rc<PayloadExtent>,
}

/// Single-threaded primary cache. No operation exposes mutable allocation state.
/// Admission returning Ok makes the value visible, not durable. Call poll
/// regularly, including during allocation pressure; retry WouldBlock admissions.
pub struct Allocator {
    diagnostics: Diagnostics,
    pressure: Arc<Admission>,
    charged: u64,
    space: Rc<Space>,
    file: uring::File,
    config: Config,
    root: Rc<Node>,
    checkpoints: [Option<Checkpoint>; 2],
    pipeline: Option<Pipeline>,
    checkpoint_permit: Option<CheckpointPermit>,
    // One bounded poll boundary for healthy maintenance before freezing a root.
    // Retained across intervening admissions; reset only by checkpoint prepare.
    // Otherwise one new fill after each yield could postpone freezing forever.
    maintenance_yielded: bool,
    changed: bool,
    rotate: bool,
    live: [usize; 2],
    metadata_count: usize,
    retired_until: [u64; 2],
    reclaim_until: u64,
    failed: bool,
    ring: Option<Rc<uring::Identity>>,
    pending: VecDeque<(Key, Weak<PayloadExtent>)>,
    publishing: VecDeque<IoTicket>,
    reads: VecDeque<Reading>,
    heat: Vec<Heat>,
    positions: HashMap<Key, usize>,
    hits: u64,
    now: u64,
    random: u64,
    // Capacity victims since the last poll, transferred to the owning worker.
    disk_cache_evictions: u64,
}
#[derive(Default)]
struct Diagnostics {
    // pending_limit, filesystem_headroom, extent_unavailable, prepared, completed.
    counts: [u64; 5],
    published: [u64; 11],
    metrics: Option<crate::metrics::Local>,
}
impl Drop for Diagnostics {
    fn drop(&mut self) {
        if let Some(metrics) = &self.metrics {
            metrics.allocator_counters(self.counts);
            metrics.allocator_state(self.published, [0; 11]);
        }
    }
}
impl Allocator {
    fn publish_diagnostics(&mut self, metrics: &crate::metrics::Local) {
        let phase = if self.failed {
            5
        } else {
            match &self.pipeline {
                None => 0,
                Some(Pipeline::Writes(_)) => 1,
                Some(Pipeline::DataSync(_)) => 2,
                Some(Pipeline::DataSynced(_)) => 3,
                Some(Pipeline::MagicWritten(_)) => 4,
            }
        };
        let mut current = [0; 11];
        current[phase] = 1;
        current[6] = self.pending.len() as u64;
        current[7] = self.publishing.len() as u64;
        current[8] = self.space.maps[Class::Payload.index()].borrow().free as u64;
        current[9] = self.charged;
        current[10] = u64::from(self.generation() < self.reclaim_until);
        let metrics = self
            .diagnostics
            .metrics
            .get_or_insert_with(|| metrics.clone());
        metrics.allocator_counters(std::mem::take(&mut self.diagnostics.counts));
        metrics.allocator_state(self.diagnostics.published, current);
        self.diagnostics.published = current;
    }
    pub fn open(
        context: &crate::workers::WorkerContext,
        shard: SlabShard,
        config: Config,
    ) -> io::Result<Self> {
        if shard.count() != context.shard_count() || !context.shard_ids().contains(&shard.id()) {
            return Err(invalid("slab shard outside worker placement"));
        }
        Self::open_inner(shard, config)
    }
    /// Consume a generation assignment without changing the pinned execution
    /// context. The legacy `open` remains restricted to startup geometry.
    pub fn open_assigned(
        context: &crate::sharding::WorkerContext,
        assignment: crate::sharding::Assignment,
        shard: SlabShard,
        config: Config,
    ) -> io::Result<Self> {
        context.check(&assignment)?;
        if shard.id() != assignment.id() || shard.count() != assignment.shard_count() {
            return Err(invalid("slab does not match storage assignment"));
        }
        Self::open_inner(shard, config)
    }
    pub(crate) fn open_inner(shard: SlabShard, config: Config) -> io::Result<Self> {
        Self::open_with(shard, config, None)
    }
    pub(crate) fn open_empty(empty: EmptyShard, config: Config) -> io::Result<Self> {
        Self::open_with(empty.shard, config, Some(empty.descriptor))
    }
    fn open_with(shard: SlabShard, config: Config, empty: Option<File>) -> io::Result<Self> {
        if config.max_pending_values == 0
            || config.max_io == 0
            || config.eviction_samples == 0
            || config.aging_interval == 0
        {
            return Err(invalid("allocator budgets must be nonzero"));
        }
        let space = Space::new(&shard);
        let mut intern = Intern::new();
        let mut checkpoints = [None, None];
        if empty.is_some() {
            for (slot, checkpoint) in checkpoints.iter_mut().enumerate() {
                *checkpoint = Some(Checkpoint {
                    generation: slot as u64 + 1,
                    root: Rc::new(Node::empty()),
                    bitmaps: Vec::new(),
                });
            }
        } else {
            // Recover newest first so an invalid older generation cannot disqualify
            // a valid latest one through allocation interning.
            let mut slots = [(0, 0), (1, 0)];
            for (slot, generation) in &mut slots {
                let mut magic = read_page(&shard.file, shard.geometry, *slot)?;
                reject_version(&magic)?;
                if valid(&mut magic, MAGIC) {
                    *generation = get(&magic, 16);
                }
            }
            slots.sort_by_key(|(_, generation)| std::cmp::Reverse(*generation));
            for (slot, _) in slots {
                match recover(&shard.file, &space, &mut intern, slot) {
                    Ok(checkpoint) => checkpoints[slot] = Some(checkpoint),
                    Err(e)
                        if matches!(
                            e.kind(),
                            io::ErrorKind::InvalidData | io::ErrorKind::UnexpectedEof
                        ) => {}
                    Err(e) => return Err(e),
                }
            }
        }
        let latest = checkpoints
            .iter()
            .flatten()
            .max_by_key(|c| c.generation)
            .ok_or_else(|| invalid("no valid shard checkpoint; explicit reformat required"))?;
        let root = latest.root.clone();
        // Only the older recovered root can contain non-live extents.
        let retired_until = [latest.generation.saturating_add(1); 2];
        // An invalid root must be durably invalidated before any of its former
        // allocations can be reused. Otherwise repaired/reused bytes could make
        // that stale root appear valid on a later restart.
        let mut invalidated = false;
        for (slot, checkpoint) in checkpoints.iter().enumerate() {
            if checkpoint.is_none() {
                shard
                    .file
                    .write_all_at(&[0; PAGE_SIZE], shard.geometry.offset(slot))?;
                invalidated = true;
            }
        }
        if invalidated {
            shard.file.sync_data()?;
        }
        let mut heat = Vec::new();
        let mut positions = HashMap::new();
        let mut live = [0; 2];
        let mut metadata_count = 0;
        root.visit(&mut |key, value| {
            match value {
                Entry::Metadata(_) => metadata_count += 1,
                Entry::Payload(_) => live[Class::Payload.index()] += 1,
            }
            positions.insert(*key, heat.len());
            heat.push(Heat {
                key: *key,
                count: 1,
                epoch: 0,
            });
        });
        Ok(Self {
            diagnostics: Diagnostics::default(),
            pressure: shard.pressure.clone(),
            charged: 0,
            file: match empty {
                Some(file) => uring::File::new(file.into()).with_slab_io(shard.file.io()),
                None => shard.file.descriptor()?,
            },
            space,
            config,
            root,
            checkpoints,
            pipeline: None,
            checkpoint_permit: None,
            maintenance_yielded: false,
            changed: false,
            rotate: false,
            live,
            metadata_count,
            retired_until,
            reclaim_until: 0,
            failed: false,
            ring: None,
            pending: VecDeque::new(),
            publishing: VecDeque::new(),
            reads: VecDeque::new(),
            heat,
            positions,
            hits: 0,
            now: 0,
            random: shard.geometry.shard + 1,
            disk_cache_evictions: 0,
        })
    }
    pub fn len(&self) -> usize {
        self.heat.len()
    }
    pub fn is_empty(&self) -> bool {
        self.heat.is_empty()
    }
    pub fn is_idle(&self) -> bool {
        self.pipeline.is_none()
            && !self.changed
            && !self.rotate
            && self.pending.is_empty()
            && self.publishing.is_empty()
    }
    pub(crate) fn maintenance_idle(&self) -> bool {
        (self.failed || self.is_idle()) && self.reads.is_empty()
    }

    pub fn generation(&self) -> u64 {
        self.checkpoints
            .iter()
            .flatten()
            .map(|c| c.generation)
            .max()
            .unwrap()
    }
    fn healthy(&self) -> io::Result<()> {
        if self.failed {
            Err(io::Error::other(
                "allocator I/O failed; quiesce and reopen slab",
            ))
        } else {
            Ok(())
        }
    }
    fn bind(&mut self, ring: &Ring) -> io::Result<()> {
        if let Some(identity) = &self.ring {
            if !Rc::ptr_eq(identity, ring.identity()) {
                return Err(invalid("foreign allocator ring"));
            }
        } else {
            self.ring = Some(ring.identity().clone());
        }
        Ok(())
    }

    /// Caller-supplied CRC64 avoids rehashing on admission. The bytes must be
    /// immutable and use the owning worker's pool when later submitted to disk.
    /// Rejection returns the original buffer for retry or uncached delivery.
    pub fn insert_payload(
        &mut self,
        key: Key,
        buffer: Buffer,
        checksum: Option<u64>,
    ) -> Result<(), uring::Rejected<Buffer>> {
        let info = ValueInfo {
            kind: Kind::Payload,
            len: buffer.as_slice().len(),
            crc64: checksum.unwrap_or(0),
            expires: 0,
        };
        let result = self.admit(key, &buffer, info, checksum);
        result.map_err(|error| uring::Rejected {
            error,
            resource: buffer,
        })
    }
    fn admit(
        &mut self,
        key: Key,
        buffer: &Buffer,
        mut info: ValueInfo,
        checksum: Option<u64>,
    ) -> io::Result<()> {
        self.healthy()?;
        validate_info(info)?;
        if self.pending.len() + self.publishing.len() >= self.config.max_pending_values {
            self.diagnostics.counts[0] = self.diagnostics.counts[0].wrapping_add(1);
            return Err(busy());
        }
        let class = Class::Payload;
        // Reject physical pressure before allocation, eviction or tree mutation.
        let bytes = WIDE;
        if let Err(error) = self.reserve_capacity(bytes) {
            self.diagnostics.counts[1] = self.diagnostics.counts[1].wrapping_add(1);
            return Err(error);
        }
        let allocation = match self.space.allocate(class) {
            Ok(a) => a,
            Err(e) => {
                self.diagnostics.counts[2] = self.diagnostics.counts[2].wrapping_add(1);
                self.release_capacity(bytes);
                self.reclaim(class, info.kind);
                return Err(e);
            }
        };
        // Hash only accepted bytes; cache pressure must not repeatedly hash a
        // multi-megabyte value on every retry.
        info.crc64 = buffer
            .checksum()
            .or(checksum)
            .unwrap_or_else(|| crc64(buffer.as_slice()));
        let value = Rc::new(PayloadExtent {
            allocation,
            info,
            buffer: RefCell::new(Some(buffer.clone())),
            written: Cell::new(false),
        });
        self.retire_key(&key);
        self.live[class.index()] += 1;
        self.put_entry(key, Entry::Payload(value.clone()));
        self.pending.push_back((key, Rc::downgrade(&value)));
        Ok(())
    }

    /// Hard per-shard bound, derived from checkpoint space and resident overhead.
    pub fn metadata_capacity(&self) -> usize {
        self.space.geometry.metadata_limit()
    }

    /// Copy metadata from the resident tree. No I/O or pool allocation.
    /// Expiry is strict: zero and expires <= now are never reusable.
    pub fn lookup_metadata(&mut self, key: &Key, now: u64) -> Option<Metadata> {
        if self.failed {
            return None;
        }
        self.now = self.now.max(now);
        let Entry::Metadata(metadata) = self.root.get(key)? else {
            return None;
        };
        if metadata.expires <= now {
            self.remove(key);
            return None;
        }
        let metadata = metadata.to_metadata();
        self.record_hit(key);
        Some(metadata)
    }

    /// Admit a reusable object record independently of any immutable payloads.
    /// Returns false for request-scoped/expired records. At capacity, bounded
    /// approximate LFU sampling prefers expired metadata; no payload is evicted.
    pub fn insert_metadata(&mut self, key: Key, metadata: Metadata, now: u64) -> io::Result<bool> {
        self.healthy()?;
        self.now = self.now.max(now);
        if metadata.expires <= now {
            return Ok(false);
        }
        // Inline records consume no value blocks, but checkpoint space must be
        // physically available before accepting mutations, just as for payloads.
        self.reserve_capacity(0)?;
        let replacing = matches!(self.root.get(&key), Some(Entry::Metadata(_)));
        if !replacing && self.metadata_count >= self.metadata_capacity() {
            self.evict_sample(Kind::Metadata, now, false)
                .ok_or_else(busy)?;
        }
        self.retire_key(&key);
        self.metadata_count += 1;
        self.put_entry(key, Entry::Metadata(metadata.into()));
        Ok(true)
    }

    fn retire_key(&mut self, key: &Key) {
        match self.root.get(key) {
            Some(Entry::Metadata(_)) => self.metadata_count -= 1,
            Some(Entry::Payload(_)) => self.retire(Class::Payload),
            None => {}
        }
    }

    fn put_entry(&mut self, key: Key, entry: Entry) {
        if let Some(right) = Node::insert(&mut self.root, key, entry) {
            self.root = Rc::new(Node {
                body: Body::Branch(vec![self.root.clone(), right]),
                disk: None,
            });
        }
        if !self.positions.contains_key(&key) {
            self.positions.insert(key, self.heat.len());
            self.heat.push(Heat {
                key,
                count: 1,
                epoch: self.hits / self.config.aging_interval,
            });
        }
        self.changed = true;
    }
    pub fn record_hit(&mut self, key: &Key) {
        if let Some(&i) = self.positions.get(key) {
            self.hits = self.hits.saturating_add(1);
            let epoch = self.hits / self.config.aging_interval;
            let heat = &mut self.heat[i];
            heat.count = ((heat.count as u32) >> (epoch - heat.epoch).min(16)) as u16;
            heat.epoch = epoch;
            heat.count = heat.count.saturating_add(1);
        }
    }
    /// Bounded anti-entropy sampling without recording an LFU hit. The lease pins
    /// this exact generation, so a concurrent replacement cannot be removed.
    pub fn scrub_candidate(&self, cursor: usize) -> Option<(Key, ReadLease)> {
        if self.failed {
            return None;
        }
        let key = self.heat.get(cursor % self.heat.len().max(1))?.key;
        Some((
            key,
            ReadLease {
                value: self.root.get(&key)?.payload()?.clone(),
            },
        ))
    }

    /// Remove only the generation that was checked by a background reader.
    pub fn remove_if_same(&mut self, key: &Key, lease: &ReadLease) -> bool {
        if self
            .root
            .get(key)
            .and_then(Entry::payload)
            .is_some_and(|value| Rc::ptr_eq(value, &lease.value))
        {
            self.remove(key)
        } else {
            false
        }
    }

    /// Read the stored bytes even if admission still retains an in-memory copy.
    /// Skip unwritten values; their checksum will be checked after checkpoint I/O.
    pub fn scrub_read(
        &mut self,
        ring: &mut Ring,
        lease: &ReadLease,
        fill: Fill,
    ) -> Result<ReadHandle, uring::Rejected<Fill>> {
        let validation = self.healthy().and_then(|_| self.bind(ring)).and_then(|_| {
            if !Rc::ptr_eq(&lease.value.allocation.space, &self.space) {
                Err(invalid("foreign scrub lease"))
            } else if !lease.value.written.get() || self.reads.len() >= self.config.max_io {
                Err(busy())
            } else {
                Ok(())
            }
        });
        if let Err(error) = validation {
            return Err(uring::Rejected {
                error,
                resource: fill,
            });
        }
        let state = Rc::new(RefCell::new(None));
        let ticket = ring.read(
            self.file.clone().into(),
            fill,
            BufferRange::new(0..lease.value.info.len).unwrap(),
            FileOffset::new(lease.value.allocation.offset()).unwrap(),
        )?;
        ring.retain(&ticket, lease.value.allocation.clone());
        self.reads.push_back(Reading {
            ticket,
            state: state.clone(),
            value: lease.value.clone(),
        });
        Ok(ReadHandle { state })
    }

    /// Look up an immutable payload lease. Metadata uses `lookup_metadata`.
    pub fn lookup(&mut self, key: &Key, now: u64) -> Option<ReadLease> {
        if self.failed {
            return None;
        }
        self.now = self.now.max(now);
        let value = self.root.get(key)?.payload()?.clone();
        self.record_hit(key);
        Some(ReadLease { value })
    }
    pub fn remove(&mut self, key: &Key) -> bool {
        if self.failed {
            return false;
        }
        let Some(_) = self.root.get(key) else {
            return false;
        };
        self.retire_key(key);
        Node::remove(&mut self.root, key);
        if self.root.len() == 0 {
            self.root = Rc::new(Node::empty());
        }
        while let Body::Branch(children) = &self.root.body {
            if children.len() != 1 {
                break;
            }
            self.root = children[0].clone();
        }
        let i = self.positions.remove(key).unwrap();
        self.heat.swap_remove(i);
        if i < self.heat.len() {
            self.positions.insert(self.heat[i].key, i);
        }
        self.changed = true;
        true
    }
}

impl Allocator {
    /// Submit through the same ring used by poll. A lease from another allocator
    /// is rejected, even when its numerical page offset happens to match.
    pub fn read(
        &mut self,
        ring: &mut Ring,
        lease: ReadLease,
        fill: Fill,
    ) -> Result<ReadHandle, uring::Rejected<Fill>> {
        let validation = self.healthy().and_then(|_| self.bind(ring)).and_then(|_| {
            if !Rc::ptr_eq(&lease.value.allocation.space, &self.space) {
                Err(invalid("foreign read lease"))
            } else if self.reads.len() >= self.config.max_io {
                Err(busy())
            } else {
                Ok(())
            }
        });
        if let Err(error) = validation {
            return Err(uring::Rejected {
                error,
                resource: fill,
            });
        }
        let state = Rc::new(RefCell::new(None));
        if let Some(buffer) = lease.buffer() {
            *state.borrow_mut() = Some(Ok(buffer));
        } else {
            let ticket = ring.read(
                self.file.clone().into(),
                fill,
                BufferRange::new(0..lease.value.info.len).unwrap(),
                FileOffset::new(lease.value.allocation.offset()).unwrap(),
            )?;
            ring.retain(&ticket, lease.value.allocation.clone());
            self.reads.push_back(Reading {
                ticket,
                state: state.clone(),
                value: lease.value,
            });
        }
        Ok(ReadHandle { state })
    }
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        self.healthy()?;
        self.bind(ring)?;
        self.publish_diagnostics(ring.metrics());
        ring.metrics()
            .disk_cache_evictions(std::mem::take(&mut self.disk_cache_evictions));
        if budget == 0 {
            return Ok(Work {
                runnable: true,
                deadline: None,
            });
        }
        let mut work = false;
        for _ in 0..budget.min(self.reads.len()) {
            let mut read = self.reads.pop_front().unwrap();
            match ring.take_read(&mut read.ticket)? {
                None => self.reads.push_back(read),
                Some(done) => {
                    let result = done.result.and_then(|n| {
                        if n != read.value.info.len {
                            return Err(invalid("short slab read"));
                        }
                        done.resource.publish_checked(n, read.value.info.crc64)
                    });
                    *read.state.borrow_mut() = Some(result);
                    work = true;
                }
            }
        }
        let mut io = RingIo {
            ring,
            file: self.file.clone(),
            space: self.space.clone(),
        };
        let result = self.progress(&mut io, budget);
        if result.is_err() {
            self.failed = true;
        } else if self.pipeline.is_none() && self.pending.is_empty() && self.publishing.is_empty() {
            // Maintenance runs only after a successful I/O boundary: the
            // pipeline latches failed while operations can error or unwind.
            self.replenish_payload_reserve();
            work |= self.changed || self.rotate;
        }
        self.publish_diagnostics(ring.metrics());
        result.map(|runnable| {
            // Pending publication and checkpoint-budget contention traditionally
            // stay runnable. A token wait must instead let the worker park.
            let deadline = ring
                .slab_deadline()
                .filter(|d| *d > crate::environment::now());
            Work {
                runnable: work || self.reads.len() > budget || (runnable && deadline.is_none()),
                deadline,
            }
        })
    }
}

#[cfg(test)]
#[path = "../tests/storage/recovery.rs"]
mod setup_tests;
#[cfg(test)]
#[path = "../tests/storage/allocator.rs"]
mod tests;
