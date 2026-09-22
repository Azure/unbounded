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
//! RACERS04 stores each object record inline in the single resident B+tree.
//! Leaves encode a 32-byte key, an 8-byte tag, then a 48-byte metadata record or
//! a payload page/length/CRC descriptor with zero padding (88 bytes total).
//! Leaf-page CRCs protect metadata; payload CRCs remain attached to extents.
//! The whole aligned eighth of each shard is reserved for index pages. Admission
//! budgets three complete trees/bitmaps (two roots plus a checkpoint in flight),
//! reserves entries for every payload extent, and caps metadata at a 16 MiB
//! resident allowance of 2 KiB per entry across CoW versions and LFU bookkeeping.
//! Metadata has no allocation class, value write, read lease, or buffer ownership.

use crate::buffers::{BUFFER_SIZE, Buffer, Fill};
use crate::metadata::Metadata;
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

// Physical admission is advisory; quarantine is the safety boundary after IO ambiguity.
use std::sync::Mutex;

const HEADROOM: u64 = 64 * 1024 * 1024;
const RESIDENT_METADATA_BUDGET: usize = 16 * 1024 * 1024;

/// Shared by every worker/shard of one slab. Outstanding admissions remain
/// charged through durable checkpoint completion, not merely buffered WRITE.
#[derive(Default)]
struct Admission(Mutex<u64>);

impl SlabFile {
    fn available_bytes(&self) -> io::Result<u64> {
        match self {
            Self::Os(file) => {
                let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
                if unsafe { libc::fstatvfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
                    return Err(io::Error::last_os_error());
                }
                let stat = unsafe { stat.assume_init() };
                Ok(stat.f_bavail.saturating_mul(stat.f_frsize))
            }
            #[cfg(test)]
            Self::Sim(disk) => Ok(disk.available_bytes()),
        }
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
        if !self
            .space
            ._file
            .available_bytes()
            .is_ok_and(|free| free >= required)
        {
            return Err(busy());
        }
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
            return Ok(Work::default());
        }
        match self.poll(ring, budget) {
            Err(error) if self.failed => {
                eprintln!(
                    "slab shard {} quarantined: {error}; restart after restoring storage capacity",
                    self.space.geometry.shard
                );
                self.pipeline = None;
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

const ATTRIBUTE: &std::ffi::CStr = c"user.racer.layout";
// Version 1: ascending round-robin shard assignment; local replica index is the
// first little-endian u64 of the key modulo the receiving worker's shard count.
const VERSION: &[u8; 8] = b"RACERL01";

#[derive(Clone, Copy)]
struct Layout {
    size: u64,
    shards: u64,
    workers: u64,
}

fn incompatible(message: impl std::fmt::Display) -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        format!(
            "slab placement layout: {message}; keep RACER_SHARDS and the actual total I/O worker count fixed (RACER_IO_WORKERS is per NUMA node; CPU affinity/topology and RACER_COMPUTE_WORKERS also affect automatic counts). No automatic migration/reformat: stop the daemon and preserve the old slab, then use a new RACER_SLAB_PATH to refill from origin; see README.md"
        ),
    )
}

impl Layout {
    fn new(size: u64, shards: usize, workers: usize) -> io::Result<Self> {
        if workers == 0 || workers > shards {
            return Err(incompatible("I/O worker count must be in 1..=shard count"));
        }
        Ok(Self {
            size,
            shards: shards as u64,
            workers: workers as u64,
        })
    }

    fn write(self, file: &File) -> io::Result<()> {
        let mut bytes = [0u8; 64];
        bytes[..8].copy_from_slice(VERSION);
        bytes[8..16].copy_from_slice(&self.size.to_le_bytes());
        bytes[16..24].copy_from_slice(&self.shards.to_le_bytes());
        bytes[24..32].copy_from_slice(&self.workers.to_le_bytes());
        let digest = blake3::hash(&bytes[..32]);
        bytes[32..].copy_from_slice(digest.as_bytes());
        // SAFETY: live fd, terminated name and readable bounded value. CREATE
        // prevents even an accidental rewrite of a previously recorded layout.
        if unsafe {
            libc::fsetxattr(
                file.as_raw_fd(),
                ATTRIBUTE.as_ptr(),
                bytes.as_ptr().cast(),
                bytes.len(),
                libc::XATTR_CREATE,
            )
        } != 0
        {
            let error = io::Error::last_os_error();
            return Err(io::Error::new(
                error.kind(),
                format!("persist slab placement xattr (user xattr support required): {error}"),
            ));
        }
        Ok(())
    }

    fn validate(self, file: &File) -> io::Result<()> {
        let mut bytes = [0u8; 64];
        // SAFETY: live locked fd, terminated name and writable bounded value.
        let len = unsafe {
            libc::fgetxattr(
                file.as_raw_fd(),
                ATTRIBUTE.as_ptr(),
                bytes.as_mut_ptr().cast(),
                bytes.len(),
            )
        };
        if len < 0 {
            let error = io::Error::last_os_error();
            return Err(match error.raw_os_error() {
                Some(libc::ENODATA) => incompatible(
                    "missing user.racer.layout metadata (legacy slab or lost xattr); historical placement cannot be inferred",
                ),
                Some(libc::ERANGE) => incompatible("oversized user.racer.layout metadata"),
                _ => error,
            });
        }
        if len != bytes.len() as isize
            || &bytes[..8] != VERSION
            || blake3::hash(&bytes[..32]).as_bytes() != &bytes[32..]
        {
            return Err(incompatible(
                "invalid or unsupported user.racer.layout metadata",
            ));
        }
        let number = |start| u64::from_le_bytes(bytes[start..start + 8].try_into().unwrap());
        let (size, shards, workers) = (number(8), number(16), number(24));
        if (size, shards, workers) != (self.size, self.shards, self.workers) {
            return Err(incompatible(format!(
                "persisted layout requires {workers} total I/O workers, {shards} shards, {size} slab bytes; startup selected {} total I/O workers, {} shards, {} slab bytes",
                self.workers, self.shards, self.size,
            )));
        }
        Ok(())
    }
}

impl Slab {
    /// Daemon startup gate. Pass the **actual** planned total I/O worker count,
    /// after CPU discovery and shard capping, not the per-NUMA environment value.
    /// Existing slabs must already carry the exact supported placement contract.
    /// New slabs publish that contract atomically with the empty checkpoints.
    /// `size` applies only to creation; existing slab length remains authoritative.
    pub fn open_or_create_layout(
        path: impl AsRef<Path>,
        size: u64,
        shards: usize,
        io_workers: usize,
    ) -> io::Result<Self> {
        // Reject invalid counts even when the path does not exist.
        let layout = Layout::new(size, shards, io_workers)?;
        match Self::open(path.as_ref(), shards) {
            Ok(slab) => {
                let file = match slab.file.as_ref() {
                    SlabFile::Os(file) => file,
                    #[cfg(test)]
                    SlabFile::Sim(_) => unreachable!("Slab::open always opens an OS file"),
                };
                Layout::new(slab.size, shards, io_workers)?.validate(file)?;
                Ok(slab)
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => Self::create_inner(
                path.as_ref(),
                size,
                shards,
                Some(layout),
                #[cfg(test)]
                |_| Ok(()),
            ),
            Err(error) => Err(error),
        }
    }
}
const WIDE: u64 = BUFFER_SIZE as u64;
const FANOUT: usize = 15;
// key (32), discriminant (8), inline metadata (48) or payload descriptor (24).
const LEAF_ENTRY: usize = 88;
const MAGIC: u64 = u64::from_le_bytes(*b"RACERS04");
const NODE: u64 = u64::from_le_bytes(*b"RACERN04");
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
// same storage object. The simulator therefore runs the real recovery parser.
pub(crate) enum SlabFile {
    Os(File),
    #[cfg(test)]
    Sim(crate::simulation::Disk),
}
impl SlabFile {
    fn read_exact_at(&self, bytes: &mut [u8], offset: u64) -> io::Result<()> {
        match self {
            Self::Os(f) => f.read_exact_at(bytes, offset),
            #[cfg(test)]
            Self::Sim(d) => d.read_exact_at(bytes, offset),
        }
    }
    fn write_all_at(&self, bytes: &[u8], offset: u64) -> io::Result<()> {
        match self {
            Self::Os(f) => f.write_all_at(bytes, offset),
            #[cfg(test)]
            Self::Sim(d) => d.write_all_at(bytes, offset),
        }
    }
    fn sync_data(&self) -> io::Result<()> {
        match self {
            Self::Os(f) => f.sync_data(),
            #[cfg(test)]
            Self::Sim(d) => d.sync_data(),
        }
    }
    fn descriptor(&self) -> io::Result<uring::File> {
        match self {
            Self::Os(f) => Ok(uring::File::new(f.try_clone()?.into())),
            #[cfg(test)]
            Self::Sim(d) => Ok(uring::File::simulated(
                crate::simulation::current()
                    .expect("simulation scope")
                    .disk(d.clone()),
            )),
        }
    }
}
#[cfg(test)]
impl Slab {
    pub(crate) fn simulated(
        disk: crate::simulation::Disk,
        size: u64,
        shards: usize,
        format: bool,
    ) -> io::Result<Self> {
        Geometry::new(size, shards, 0)?;
        let file = SlabFile::Sim(disk);
        if format {
            for shard in 0..shards {
                let g = Geometry::new(size, shards, shard)?;
                for slot in 0..2 {
                    file.write_all_at(&magic(g, slot as u64 + 1, 0, &[]).0, g.offset(slot))?;
                }
            }
            file.sync_data()?;
        }
        Ok(Self {
            file: Arc::new(file),
            pressure: Arc::default(),
            size,
            shards: vec![false; shards],
        })
    }
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn reject_version(page: &uring::Page) -> io::Result<()> {
    if page.0[..6] == *b"RACERS" && get(page, 0) != MAGIC {
        return Err(invalid(
            "incompatible slab format: expected RACERS04 inline metadata; preserve the old slab and use a new RACER_SLAB_PATH; no automatic migration/reformat",
        ));
    }
    Ok(())
}
fn busy() -> io::Error {
    io::ErrorKind::WouldBlock.into()
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
                "slab size must be a multiple of 4 MiB and at most i64::MAX bytes",
            ));
        }
        let len = size / count as u64 / WIDE * WIDE;
        if len < 8 * WIDE {
            return Err(invalid(
                "each shard needs at least 32 MiB; increase slab size or reduce shard count for a new slab",
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
                    "slab geometry: {size} bytes across {count} shards gives {len} bytes/shard requiring {bitmap_pages} bitmap pages; RACERS04 roots hold at most {ROOT_BITMAP_SLOTS}, limiting each shard to {MAX_SHARD_SIZE} bytes (4 MiB aligned). Reduce slab size or increase RACER_SHARDS for a new slab; preserve an existing slab and use a new RACER_SLAB_PATH rather than changing its layout"
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
            Class::Payload => (end, (self.pages() - end) / 1024, 1024),
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
impl Slab {
    /// Format privately, then atomically publish without replacing any existing
    /// name. An error after publication (including directory sync failure) can
    /// leave a valid slab; retry by opening it, never by reformatting it.
    pub fn create(path: impl AsRef<Path>, size: u64, shards: usize) -> io::Result<Self> {
        Self::create_inner(
            path.as_ref(),
            size,
            shards,
            None,
            #[cfg(test)]
            |_| Ok(()),
        )
    }
    fn create_inner(
        path: &Path,
        size: u64,
        shards: usize,
        layout: Option<Layout>,
        #[cfg(test)] mut step: impl FnMut(CreateStep) -> io::Result<()>,
    ) -> io::Result<Self> {
        Geometry::new(size, shards, 0)?;
        let mut temporary = TemporarySlab::new(path)?;
        let file = &temporary.file;
        lock(file)?;
        validate_page_cache_storage(file)?;
        #[cfg(test)]
        step(CreateStep::Created)?;
        file.set_len(size)?;
        #[cfg(test)]
        step(CreateStep::Sized)?;
        // Initial empty checkpoints contain no bitmap pages; recovery accepts
        // this special case only for an empty root.
        for shard in 0..shards {
            let g = Geometry::new(size, shards, shard)?;
            for slot in 0..2 {
                file.write_all_at(&magic(g, slot as u64 + 1, 0, &[]).0, g.offset(slot))?;
                #[cfg(test)]
                step(CreateStep::Checkpoint(shard, slot))?;
            }
        }
        if let Some(layout) = layout {
            layout.write(file)?;
            #[cfg(test)]
            step(CreateStep::LayoutWritten)?;
        }
        #[cfg(test)]
        step(CreateStep::BeforeFileSync)?;
        file.sync_all()?;
        #[cfg(test)]
        step(CreateStep::FileSynced)?;
        temporary.publish()?;
        #[cfg(test)]
        step(CreateStep::Published)?;
        temporary.directory.sync_all()?;
        #[cfg(test)]
        step(CreateStep::DirectorySynced)?;
        Ok(Self {
            // The clone shares the locked open file description: publication
            // and transfer to shard owners never introduce an unlocked window.
            file: Arc::new(SlabFile::Os(temporary.file.try_clone()?)),
            pressure: Arc::default(),
            size,
            shards: vec![false; shards],
        })
    }
    pub fn open(path: impl AsRef<Path>, shards: usize) -> io::Result<Self> {
        let file = OpenOptions::new().read(true).write(true).open(path)?;
        lock(&file)?;
        validate_page_cache_storage(&file)?;
        let size = file.metadata()?.len();
        Geometry::new(size, shards, 0)?;
        for shard in 0..shards {
            let g = Geometry::new(size, shards, shard)?;
            for slot in 0..2 {
                let mut page = uring::Page([0; PAGE_SIZE]);
                file.read_exact_at(&mut page.0, g.offset(slot))?;
                reject_version(&page)?;
            }
        }
        Ok(Self {
            file: Arc::new(SlabFile::Os(file)),
            pressure: Arc::default(),
            size,
            shards: vec![false; shards],
        })
    }
    pub fn take_shard(&mut self, id: ShardId) -> io::Result<SlabShard> {
        let g = Geometry::new(self.size, self.shards.len(), id.index())?;
        if std::mem::replace(&mut self.shards[id.index()], true) {
            return Err(invalid("shard capability already issued"));
        }
        Ok(SlabShard {
            file: self.file.clone(),
            pressure: self.pressure.clone(),
            geometry: g,
        })
    }
}

#[cfg(test)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum CreateStep {
    Created,
    Sized,
    Checkpoint(usize, usize),
    LayoutWritten,
    BeforeFileSync,
    FileSynced,
    Published,
    DirectorySynced,
}

// Anchor every namespace operation to one directory descriptor, even if an
// ancestor is renamed during setup. A killed creator may leave a private temp
// name, but it cannot leave an incomplete final slab or block the next create.
struct TemporarySlab {
    directory: File,
    name: CString,
    destination: CString,
    file: File,
    published: bool,
}
impl TemporarySlab {
    fn new(path: &Path) -> io::Result<Self> {
        let destination = path
            .file_name()
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "missing slab filename"))?;
        let destination = CString::new(destination.as_bytes())?;
        let parent = path.parent().filter(|p| !p.as_os_str().is_empty());
        let directory = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_DIRECTORY | libc::O_CLOEXEC)
            .open(parent.unwrap_or_else(|| Path::new(".")))?;
        loop {
            let mut random = [0; 16];
            getrandom::getrandom(&mut random).map_err(|e| io::Error::other(e.to_string()))?;
            let name = CString::new(format!(
                ".racer-slab-{:032x}.tmp",
                u128::from_ne_bytes(random)
            ))
            .unwrap();
            // SAFETY: live directory fd, terminated name, and mode supplied for
            // O_CREAT. O_EXCL never follows or reuses a competing temporary file.
            let fd = unsafe {
                libc::openat(
                    directory.as_raw_fd(),
                    name.as_ptr(),
                    libc::O_RDWR | libc::O_CREAT | libc::O_EXCL | libc::O_CLOEXEC,
                    0o666 as libc::mode_t,
                )
            };
            if fd >= 0 {
                return Ok(Self {
                    directory,
                    name,
                    destination,
                    // SAFETY: openat returned a new owned descriptor.
                    file: unsafe { File::from_raw_fd(fd) },
                    published: false,
                });
            }
            let error = io::Error::last_os_error();
            if error.kind() != io::ErrorKind::AlreadyExists {
                return Err(error);
            }
        }
    }
    fn publish(&mut self) -> io::Result<()> {
        // SAFETY: both names are terminated and the directory descriptor is live.
        // Unlike rename(), NOREPLACE also protects dangling symlinks and racing
        // creators. Never fall back to an overwriting rename.
        if unsafe {
            libc::renameat2(
                self.directory.as_raw_fd(),
                self.name.as_ptr(),
                self.directory.as_raw_fd(),
                self.destination.as_ptr(),
                libc::RENAME_NOREPLACE,
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        self.published = true;
        Ok(())
    }
}
impl Drop for TemporarySlab {
    fn drop(&mut self) {
        if !self.published {
            // SAFETY: live directory descriptor and terminated private name.
            // Best effort on error/unwind; never unlink the published slab.
            unsafe { libc::unlinkat(self.directory.as_raw_fd(), self.name.as_ptr(), 0) };
        }
    }
}
// Full-page hole punching must detach, rather than zero, pages retained by TCP.
// The supported deployment baseline is ext4 with 4 KiB base pages.
fn validate_page_cache_storage(file: &File) -> io::Result<()> {
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: live descriptor and writable statfs storage.
    if unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let stat = unsafe { stat.assume_init() };
    if stat.f_type != 0xef53 || unsafe { libc::sysconf(libc::_SC_PAGESIZE) } != PAGE_SIZE as i64 {
        return Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "page-cache slabs require ext4 and 4 KiB base pages",
        ));
    }
    Ok(())
}
fn lock(file: &File) -> io::Result<()> {
    // SAFETY: flock borrows a live descriptor and retains no userspace pointers.
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
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
        let mut this = Self {
            words: vec![0; count.div_ceil(64)],
            summary: vec![0; count.div_ceil(4096)],
            free: 0,
        };
        for i in 0..count {
            this.release(i);
        }
        this
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
impl Space {
    fn new(shard: &SlabShard) -> Rc<Self> {
        Rc::new(Self {
            geometry: shard.geometry,
            maps: [Class::Index, Class::Payload]
                .map(|c| RefCell::new(Bitmap::new(shard.geometry.range(c).1))),
            _file: shard.file.clone(),
            retired: RefCell::new(Vec::new()),
        })
    }
    fn allocate(self: &Rc<Self>, class: Class) -> io::Result<Rc<Allocation>> {
        self.retired.borrow_mut().retain(|(class, index, pin)| {
            if pin.strong_count() == 0 {
                self.maps[class.index()].borrow_mut().release(*index);
                false
            } else {
                true
            }
        });
        let index = self.maps[class.index()]
            .borrow_mut()
            .take()
            .ok_or_else(busy)?;
        Ok(Rc::new(Allocation {
            space: self.clone(),
            class,
            index,
            pin: Arc::new(()),
        }))
    }
}
struct Allocation {
    space: Rc<Space>,
    class: Class,
    index: usize,
    pin: Arc<()>,
}
impl Allocation {
    fn page(&self) -> usize {
        let (start, _, stride) = self.space.geometry.range(self.class);
        start + self.index * stride
    }
    fn offset(&self) -> u64 {
        self.space.geometry.offset(self.page())
    }
}
impl Drop for Allocation {
    fn drop(&mut self) {
        if Arc::strong_count(&self.pin) != 1 {
            self.space.retired.borrow_mut().push((
                self.class,
                self.index,
                Arc::downgrade(&self.pin),
            ));
            return;
        }
        self.space.maps[self.class.index()]
            .borrow_mut()
            .release(self.index);
    }
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
    Metadata(Metadata),
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
impl Node {
    fn empty() -> Self {
        Self {
            body: Body::Leaf(Vec::new()),
            disk: None,
        }
    }
    fn first(&self) -> Key {
        match &self.body {
            Body::Leaf(v) => v.first().map_or([0; 32], |v| v.0),
            Body::Branch(v) => v[0].first(),
        }
    }
    fn len(&self) -> usize {
        match &self.body {
            Body::Leaf(v) => v.len(),
            Body::Branch(v) => v.len(),
        }
    }
    fn height(&self) -> usize {
        match &self.body {
            Body::Leaf(_) => 0,
            Body::Branch(v) => 1 + v[0].height(),
        }
    }
    fn child(children: &[Rc<Node>], key: &Key) -> usize {
        children
            .partition_point(|c| c.first() <= *key)
            .saturating_sub(1)
    }
    fn get(&self, key: &Key) -> Option<&Entry> {
        match &self.body {
            Body::Leaf(v) => v.binary_search_by_key(key, |v| v.0).ok().map(|i| &v[i].1),
            Body::Branch(v) => v[Self::child(v, key)].get(key),
        }
    }
    fn insert(node: &mut Rc<Self>, key: Key, value: Entry) -> Option<Rc<Self>> {
        let node = Rc::make_mut(node);
        node.disk = None;
        match &mut node.body {
            Body::Leaf(v) => match v.binary_search_by_key(&key, |v| v.0) {
                Ok(i) => v[i].1 = value,
                Err(i) => v.insert(i, (key, value)),
            },
            Body::Branch(v) => {
                let i = Self::child(v, &key);
                if let Some(right) = Self::insert(&mut v[i], key, value) {
                    v.insert(i + 1, right);
                }
            }
        }
        if node.len() <= FANOUT {
            return None;
        }
        let body = match &mut node.body {
            Body::Leaf(v) => Body::Leaf(v.split_off(v.len() / 2)),
            Body::Branch(v) => Body::Branch(v.split_off(v.len() / 2)),
        };
        Some(Rc::new(Self { body, disk: None }))
    }
    fn remove(node: &mut Rc<Self>, key: &Key) -> bool {
        if node.get(key).is_none() {
            return false;
        }
        let node = Rc::make_mut(node);
        node.disk = None;
        match &mut node.body {
            Body::Leaf(v) => {
                v.remove(v.binary_search_by_key(key, |v| v.0).unwrap());
            }
            Body::Branch(v) => {
                let i = Self::child(v, key);
                Self::remove(&mut v[i], key);
                if v[i].len() == 0 {
                    v.remove(i);
                } else if v.len() > 1 {
                    let left = i.min(v.len() - 2);
                    if v[i].len() < FANOUT.div_ceil(2) {
                        let right = v.remove(left + 1);
                        let target = Rc::make_mut(&mut v[left]);
                        target.disk = None;
                        match (&mut target.body, &right.body) {
                            (Body::Leaf(a), Body::Leaf(b)) => a.extend(b.iter().cloned()),
                            (Body::Branch(a), Body::Branch(b)) => a.extend(b.iter().cloned()),
                            _ => unreachable!(),
                        }
                        if target.len() > FANOUT {
                            let body = match &mut target.body {
                                Body::Leaf(a) => Body::Leaf(a.split_off(a.len() / 2)),
                                Body::Branch(a) => Body::Branch(a.split_off(a.len() / 2)),
                            };
                            v.insert(left + 1, Rc::new(Node { body, disk: None }));
                        }
                    }
                }
            }
        }
        true
    }
    fn visit(&self, f: &mut impl FnMut(&Key, &Entry)) {
        match &self.body {
            Body::Leaf(v) => v.iter().for_each(|(k, v)| f(k, v)),
            Body::Branch(v) => v.iter().for_each(|v| v.visit(f)),
        }
    }
}

fn put(page: &mut uring::Page, at: usize, value: u64) {
    page.0[at..at + 8].copy_from_slice(&value.to_le_bytes());
}
fn get(page: &uring::Page, at: usize) -> u64 {
    u64::from_le_bytes(page.0[at..at + 8].try_into().unwrap())
}

// CRC64/ECMA-182 with runtime CPU dispatch and a software fallback.
// Values may supply an already computed checksum.
pub fn crc64(bytes: &[u8]) -> u64 {
    crc_fast::checksum(crc_fast::CrcAlgorithm::Crc64Ecma182, bytes)
}
fn seal(page: &mut uring::Page) {
    put(page, 8, 0);
    put(page, 8, crc64(&page.0));
}
fn valid(page: &mut uring::Page, tag: u64) -> bool {
    let checksum = get(page, 8);
    put(page, 8, 0);
    let ok = get(page, 0) == tag && crc64(&page.0) == checksum;
    put(page, 8, checksum);
    ok
}
fn magic(g: Geometry, generation: u64, root: u64, bitmaps: &[Rc<Allocation>]) -> Box<uring::Page> {
    let mut page = Box::new(uring::Page([0; PAGE_SIZE]));
    for (i, value) in [
        MAGIC,
        0,
        generation,
        g.base,
        g.len,
        g.shard,
        g.count,
        root,
        bitmaps.len() as u64,
    ]
    .into_iter()
    .enumerate()
    {
        put(&mut page, i * 8, value);
    }
    for (i, bitmap) in bitmaps.iter().enumerate() {
        put(&mut page, ROOT_HEADER_BYTES + i * 8, bitmap.page() as u64);
    }
    seal(&mut page);
    page
}
fn encode(node: &Node) -> Box<uring::Page> {
    let mut page = Box::new(uring::Page([0; PAGE_SIZE]));
    put(&mut page, 0, NODE);
    put(
        &mut page,
        16,
        u64::from(matches!(node.body, Body::Branch(_))),
    );
    put(&mut page, 24, node.len() as u64);
    match &node.body {
        Body::Leaf(v) => {
            for (i, (key, value)) in v.iter().enumerate() {
                let at = 32 + i * LEAF_ENTRY;
                page.0[at..at + 32].copy_from_slice(key);
                match value {
                    Entry::Metadata(metadata) => {
                        page.0[at + 40..at + LEAF_ENTRY].copy_from_slice(&metadata.to_bytes());
                    }
                    Entry::Payload(value) => {
                        put(&mut page, at + 32, 1);
                        put(&mut page, at + 40, value.allocation.page() as u64);
                        put(&mut page, at + 48, value.info.len as u64);
                        put(&mut page, at + 56, value.info.crc64);
                    }
                }
            }
        }
        Body::Branch(v) => {
            for (i, child) in v.iter().enumerate() {
                let at = 32 + i * 40;
                page.0[at..at + 32].copy_from_slice(&child.first());
                put(
                    &mut page,
                    at + 32,
                    child.disk.as_ref().unwrap().page() as u64,
                );
            }
        }
    }
    seal(&mut page);
    page
}

struct Checkpoint {
    generation: u64,
    root: Rc<Node>,
    bitmaps: Vec<(Rc<Allocation>, Box<uring::Page>)>,
}

// Recovery interns allocations shared by the two checkpoints. A tag prevents
// the same physical extent from being accepted with conflicting identities.
type Intern = HashMap<usize, (u64, Weak<Allocation>)>;
fn claim(
    space: &Rc<Space>,
    intern: &mut Intern,
    page: usize,
    class: Class,
    tag: u64,
) -> io::Result<Rc<Allocation>> {
    let (start, count, stride) = space.geometry.range(class);
    if page < start || !(page - start).is_multiple_of(stride) || (page - start) / stride >= count {
        return Err(invalid("allocation outside its size class"));
    }
    if let Some((old, allocation)) = intern.get(&page)
        && let Some(allocation) = allocation.upgrade()
    {
        if *old != tag || allocation.class != class {
            return Err(invalid("conflicting checkpoint allocations"));
        }
        return Ok(allocation);
    }
    let index = (page - start) / stride;
    space.maps[class.index()].borrow_mut().set(index, false);
    let allocation = Rc::new(Allocation {
        space: space.clone(),
        class,
        index,
        pin: Arc::new(()),
    });
    intern.insert(page, (tag, Rc::downgrade(&allocation)));
    Ok(allocation)
}
fn read_page(file: &SlabFile, g: Geometry, page: usize) -> io::Result<Box<uring::Page>> {
    if page >= g.pages() {
        return Err(invalid("page outside shard"));
    }
    let mut bytes = Box::new(uring::Page([0; PAGE_SIZE]));
    file.read_exact_at(&mut bytes.0, g.offset(page))?;
    Ok(bytes)
}
fn mark(bits: &mut [u8], page: usize, count: usize) -> io::Result<()> {
    for i in page..page + count {
        let mask = 1 << (i % 8);
        if bits[i / 8] & mask != 0 {
            return Err(invalid("overlapping or cyclic checkpoint"));
        }
        bits[i / 8] |= mask;
    }
    Ok(())
}
fn load_node(
    file: &SlabFile,
    space: &Rc<Space>,
    intern: &mut Intern,
    used: &mut [u8],
    page: usize,
    depth: usize,
    metadata_remaining: &mut usize,
) -> io::Result<Rc<Node>> {
    if depth > 16 {
        return Err(invalid("tree too deep"));
    }
    let mut bytes = read_page(file, space.geometry, page)?;
    if !valid(&mut bytes, NODE) {
        return Err(invalid("invalid tree checksum"));
    }
    let allocation = claim(space, intern, page, Class::Index, get(&bytes, 8))?;
    mark(used, page, 1)?;
    let count = get(&bytes, 24) as usize;
    if count == 0 || count > FANOUT || (depth != 0 && count < FANOUT.div_ceil(2)) {
        return Err(invalid("invalid tree occupancy"));
    }
    let mut previous = None;
    let body = match get(&bytes, 16) {
        0 => {
            let mut values = Vec::with_capacity(count);
            for i in 0..count {
                let at = 32 + i * LEAF_ENTRY;
                let key: Key = bytes.0[at..at + 32].try_into().unwrap();
                if previous.is_some_and(|p| p >= key) {
                    return Err(invalid("unsorted leaf"));
                }
                previous = Some(key);
                let value = match get(&bytes, at + 32) {
                    0 => {
                        *metadata_remaining = metadata_remaining
                            .checked_sub(1)
                            .ok_or_else(|| invalid("checkpoint exceeds metadata entry bound"))?;
                        let metadata = Metadata::from_bytes(&bytes.0[at + 40..at + LEAF_ENTRY])?;
                        if metadata.expires == 0 {
                            return Err(invalid("request-scoped metadata in checkpoint"));
                        }
                        Entry::Metadata(metadata)
                    }
                    1 => {
                        if bytes.0[at + 64..at + LEAF_ENTRY].iter().any(|b| *b != 0) {
                            return Err(invalid("nonzero payload descriptor padding"));
                        }
                        let info = ValueInfo {
                            kind: Kind::Payload,
                            len: get(&bytes, at + 48) as usize,
                            crc64: get(&bytes, at + 56),
                            expires: 0,
                        };
                        validate_info(info)?;
                        let allocation = claim(
                            space,
                            intern,
                            get(&bytes, at + 40) as usize,
                            Class::Payload,
                            crc64(&bytes.0[at..at + LEAF_ENTRY]),
                        )?;
                        mark(used, allocation.page(), 1024)?;
                        Entry::Payload(Rc::new(PayloadExtent {
                            allocation,
                            info,
                            buffer: RefCell::new(None),
                            written: Cell::new(true),
                        }))
                    }
                    _ => return Err(invalid("invalid value kind")),
                };
                values.push((key, value));
            }
            Body::Leaf(values)
        }
        1 => {
            if count < 2 {
                return Err(invalid("unary tree root"));
            }
            let mut children = Vec::with_capacity(count);
            for i in 0..count {
                let at = 32 + i * 40;
                let key: Key = bytes.0[at..at + 32].try_into().unwrap();
                if previous.is_some_and(|p| p >= key) {
                    return Err(invalid("unsorted branch"));
                }
                let child = load_node(
                    file,
                    space,
                    intern,
                    used,
                    get(&bytes, at + 32) as usize,
                    depth + 1,
                    metadata_remaining,
                )?;
                if child.first() != key {
                    return Err(invalid("invalid branch separator"));
                }
                if children
                    .first()
                    .is_some_and(|c: &Rc<Node>| c.height() != child.height())
                {
                    return Err(invalid("unbalanced tree"));
                }
                previous = Some(key);
                children.push(child);
            }
            Body::Branch(children)
        }
        _ => return Err(invalid("invalid tree kind")),
    };
    Ok(Rc::new(Node {
        body,
        disk: Some(allocation),
    }))
}
fn validate_info(info: ValueInfo) -> io::Result<()> {
    if info.len == 0 || info.len > BUFFER_SIZE || info.kind != Kind::Payload || info.expires != 0 {
        return Err(invalid("invalid cache value length or expiration"));
    }
    Ok(())
}

fn recover(
    file: &SlabFile,
    space: &Rc<Space>,
    intern: &mut Intern,
    slot: usize,
) -> io::Result<Checkpoint> {
    let g = space.geometry;
    let mut page = read_page(file, g, slot)?;
    if !valid(&mut page, MAGIC)
        || get(&page, 16) == 0
        || [
            get(&page, 24),
            get(&page, 32),
            get(&page, 40),
            get(&page, 48),
        ] != [g.base, g.len, g.shard, g.count]
    {
        return Err(invalid("invalid shard magic or geometry"));
    }
    let mut used = vec![0u8; g.pages().div_ceil(8)];
    let mut metadata_remaining = g.metadata_limit();
    let root = if get(&page, 56) == 0 {
        Rc::new(Node::empty())
    } else {
        load_node(
            file,
            space,
            intern,
            &mut used,
            get(&page, 56) as usize,
            0,
            &mut metadata_remaining,
        )?
    };
    // Check cross-child ordering as well as local separator ordering.
    let mut previous = None;
    let mut sorted = true;
    root.visit(&mut |key, _| {
        sorted &= previous.is_none_or(|p| p < *key);
        previous = Some(*key);
    });
    if !sorted {
        return Err(invalid("overlapping tree key ranges"));
    }
    let count = get(&page, 64) as usize;
    if count > ROOT_BITMAP_SLOTS
        || (count != used.len().div_ceil(BIT_BYTES) && !(count == 0 && root.len() == 0))
    {
        return Err(invalid("invalid bitmap length"));
    }
    let mut bitmaps = Vec::with_capacity(count);
    for i in 0..count {
        let position = get(&page, ROOT_HEADER_BYTES + i * 8) as usize;
        let mut bitmap = read_page(file, g, position)?;
        if !valid(&mut bitmap, BITS) || get(&bitmap, 16) != i as u64 {
            return Err(invalid("invalid bitmap page"));
        }
        let start = i * BIT_BYTES;
        let len = BIT_BYTES.min(used.len() - start);
        if bitmap.0[32..32 + len] != used[start..start + len] {
            return Err(invalid("bitmap disagrees with tree"));
        }
        let allocation = claim(space, intern, position, Class::Index, get(&bitmap, 8))?;
        // Separate bitmap-page set: bitmap bits describe tree and values only.
        if used[position / 8] & (1 << (position % 8)) != 0
            || bitmaps
                .iter()
                .any(|(a, _): &(Rc<Allocation>, Box<uring::Page>)| a.page() == position)
        {
            return Err(invalid("bitmap overlaps checkpoint"));
        }
        bitmaps.push((allocation, bitmap));
    }
    Ok(Checkpoint {
        generation: get(&page, 16),
        root,
        bitmaps,
    })
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
    #[cfg(test)]
    pub(crate) fn simulation_id(&self) -> Option<i32> {
        self.descriptor.simulation_id()
    }
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
    pressure: Arc<Admission>,
    charged: u64,
    space: Rc<Space>,
    file: uring::File,
    config: Config,
    root: Rc<Node>,
    checkpoints: [Option<Checkpoint>; 2],
    pipeline: Option<Pipeline>,
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
}
impl Allocator {
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
    pub(crate) fn open_inner(shard: SlabShard, config: Config) -> io::Result<Self> {
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
            pressure: shard.pressure.clone(),
            charged: 0,
            file: shard.file.descriptor()?,
            space,
            config,
            root,
            checkpoints,
            pipeline: None,
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
    #[cfg(test)]
    pub(crate) fn pressure_snapshot(&self) -> String {
        let mut pinned = 0;
        self.root.visit(&mut |_, value| {
            if let Entry::Payload(value) = value {
                pinned += usize::from(Arc::strong_count(&value.allocation.pin) > 1);
            }
        });
        format!(
            "idle={} live={:?} free={:?} file_pinned={pinned} retired_pins={} pending={} publishing={} generation={} reclaim_until={}",
            self.is_idle(),
            self.live,
            self.space.maps.each_ref().map(|m| m.borrow().free),
            self.space
                .retired
                .borrow()
                .iter()
                .filter(|(_, _, p)| p.strong_count() != 0)
                .count(),
            self.pending.len(),
            self.publishing.len(),
            self.generation(),
            self.reclaim_until
        )
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
            return Err(busy());
        }
        let class = Class::Payload;
        // Reject physical pressure before allocation, eviction or tree mutation.
        let bytes = WIDE;
        self.reserve_capacity(bytes)?;
        let allocation = match self.space.allocate(class) {
            Ok(a) => a,
            Err(e) => {
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

    /// Copy inline metadata from the resident tree. No I/O or pool allocation.
    /// Expiry is strict: zero and expires <= now are never reusable.
    pub fn lookup_metadata(&mut self, key: &Key, now: u64) -> Option<Metadata> {
        if self.failed {
            return None;
        }
        self.now = self.now.max(now);
        let Entry::Metadata(metadata) = self.root.get(key)? else {
            return None;
        };
        let metadata = *metadata;
        if metadata.expires <= now {
            self.remove(key);
            return None;
        }
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
        self.put_entry(key, Entry::Metadata(metadata));
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
    fn retire(&mut self, class: Class) {
        self.live[class.index()] -= 1;
        // An in-flight snapshot may still contain this version. Two subsequent
        // publications exclude it from BOTH roots; leases can hold it longer.
        self.retired_until[class.index()] = self
            .generation()
            .saturating_add(2 + u64::from(self.pipeline.is_some()));
    }
    fn reclaim_target(&self, class: Class) -> usize {
        (self.space.geometry.range(class).1 / 4)
            .max(1)
            .min(RECLAIM_BATCH)
            .min(self.config.max_pending_values)
    }
    fn reclaim(&mut self, class: Class, kind: Kind) {
        let index = class.index();
        let capacity = self.space.geometry.range(class).1;
        // Non-live extents include replacements, explicit removals, snapshots,
        // outstanding kernel requests and leases. Count them toward the target
        // even when none are physically free yet: retries must not evict another
        // batch while that space is pinned.
        let missing = self
            .reclaim_target(class)
            .saturating_sub(capacity - self.live[index]);
        for _ in 0..missing {
            if self.evict_sample(kind, self.now, true).is_none() {
                break;
            }
        }
        if capacity - self.live[index] > self.space.maps[index].borrow().free {
            self.reclaim_until = self.reclaim_until.max(self.retired_until[index]);
            // Count the already scheduled publication too. A retry during its
            // final sync must not schedule an unnecessary third rotation.
            let scheduled = self
                .generation()
                .saturating_add(u64::from(self.pipeline.is_some()));
            self.rotate |= scheduled < self.reclaim_until;
        }
    }
    /// Bounded approximate LFU. A removed victim can remain physically pinned
    /// until the older checkpoint rotates out; poll then retry admission.
    pub fn evict(&mut self, kind: Kind, now: u64) -> Option<Key> {
        self.evict_sample(kind, now, false)
    }
    fn evict_sample(&mut self, kind: Kind, now: u64, durable_only: bool) -> Option<Key> {
        if self.failed {
            return None;
        }
        self.now = self.now.max(now);
        if self.heat.is_empty() {
            return None;
        }
        let epoch = self.hits / self.config.aging_interval;
        let mut best = None;
        for _ in 0..self.config.eviction_samples {
            self.random ^= self.random << 13;
            self.random ^= self.random >> 7;
            self.random ^= self.random << 17;
            let i = self.random as usize % self.heat.len();
            let heat = &self.heat[i];
            let value = self.root.get(&heat.key).unwrap();
            if value.kind() != kind {
                continue;
            }
            // A response can retain this file while awaiting another page's
            // admission. Evicting it cannot release its extent and would fill
            // the reclaim quota with a pin whose release depends on admission.
            // Explicit eviction still permits retiring a pinned generation.
            if durable_only
                && value
                    .payload()
                    .is_some_and(|v| Arc::strong_count(&v.allocation.pin) > 1)
            {
                continue;
            }
            if durable_only
                && !self.checkpoints.iter().flatten().any(|c| {
                    c.root
                        .get(&heat.key)
                        .and_then(Entry::payload)
                        .zip(value.payload())
                        .is_some_and(|(old, value)| Rc::ptr_eq(&old.allocation, &value.allocation))
                })
            {
                // Written is insufficient: the snapshot's final sync may still
                // be pending. Never churn uncheckpointed admissions for space.
                continue;
            }
            let count = if matches!(value, Entry::Metadata(m) if m.expires <= now) {
                0
            } else {
                1 + (heat.count as u32 >> (epoch - heat.epoch).min(16))
            };
            if best.is_none_or(|(_, score)| count < score) {
                best = Some((heat.key, count));
            }
        }
        let key = best?.0;
        self.remove(&key);
        Some(key)
    }

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
        }
        result.map(|runnable| Work {
            runnable: runnable || work || self.reads.len() > budget,
            deadline: None,
        })
    }
}

enum Job {
    Page(Box<uring::Page>, u64),
    Value(Rc<PayloadExtent>),
    Sync,
}
trait Storage {
    type Ticket;
    fn submit(&mut self, job: Job) -> Result<Self::Ticket, uring::Rejected<Job>>;
    fn complete(&mut self, ticket: &mut Self::Ticket) -> io::Result<Option<io::Result<()>>>;
}
enum IoTicket {
    Page(uring::Ticket<uring::PageIo>),
    Punch(uring::Ticket<uring::PunchHole>, Rc<PayloadExtent>),
    Detached(Rc<PayloadExtent>),
    Value(uring::Ticket<uring::Write>, Rc<PayloadExtent>),
    Sync(uring::Ticket<uring::Control>),
    #[cfg(test)]
    Sim(usize),
}
struct RingIo<'a> {
    ring: &'a mut Ring,
    file: uring::File,
    space: Rc<Space>,
}
impl Storage for RingIo<'_> {
    type Ticket = IoTicket;
    fn submit(&mut self, job: Job) -> Result<IoTicket, uring::Rejected<Job>> {
        match job {
            Job::Page(page, offset) => match self.ring.write_page(
                self.file.clone().into(),
                page,
                FileOffset::new(offset).unwrap(),
            ) {
                Ok(ticket) => {
                    self.ring.retain(&ticket, self.space.clone());
                    Ok(IoTicket::Page(ticket))
                }
                Err(e) => Err(uring::Rejected {
                    error: e.error,
                    resource: Job::Page(e.resource, offset),
                }),
            },
            Job::Value(value) => {
                match self.ring.punch_hole(
                    self.file.clone().into(),
                    FileOffset::new(value.allocation.offset()).unwrap(),
                    WIDE,
                    value.allocation.clone(),
                ) {
                    Ok(ticket) => Ok(IoTicket::Punch(ticket, value)),
                    Err(error) => Err(uring::Rejected {
                        error,
                        resource: Job::Value(value),
                    }),
                }
            }
            Job::Sync => match self.ring.sync_data(self.file.clone().into()) {
                Ok(ticket) => {
                    self.ring.retain(&ticket, self.space.clone());
                    Ok(IoTicket::Sync(ticket))
                }
                Err(error) => Err(uring::Rejected {
                    error,
                    resource: Job::Sync,
                }),
            },
        }
    }
    fn complete(&mut self, ticket: &mut IoTicket) -> io::Result<Option<io::Result<()>>> {
        fn exact(result: io::Result<usize>, len: usize) -> io::Result<()> {
            if result? != len {
                return Err(io::Error::other("short slab write or invalid sync result"));
            }
            Ok(())
        }
        Ok(match ticket {
            IoTicket::Punch(t, value) => {
                match self.ring.take_punch(t)? {
                    None => return Ok(None),
                    Some(Err(error)) => return Ok(Some(Err(error))),
                    Some(Ok(())) => *ticket = IoTicket::Detached(value.clone()),
                }
                self.complete(ticket)?
            }
            IoTicket::Detached(value) => {
                let buffer = value.buffer.borrow().as_ref().unwrap().clone();
                match self.ring.write(
                    self.file.clone().into(),
                    buffer,
                    BufferRange::new(0..value.info.len).unwrap(),
                    FileOffset::new(value.allocation.offset()).unwrap(),
                ) {
                    Ok(t) => {
                        self.ring.retain(&t, value.allocation.clone());
                        *ticket = IoTicket::Value(t, value.clone());
                        None
                    }
                    Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => None,
                    Err(e) => Some(Err(e.error)),
                }
            }
            IoTicket::Page(t) => self.ring.take_page(t)?.map(|c| exact(c.result, PAGE_SIZE)),
            IoTicket::Value(t, value) => self.ring.take_write(t)?.map(|c| {
                exact(c.result, value.info.len)?;
                value.written.set(true);
                value.buffer.borrow_mut().take();
                Ok(())
            }),
            IoTicket::Sync(t) => self.ring.take_control(t)?.map(|c| exact(c.result, 0)),
            #[cfg(test)]
            IoTicket::Sim(_) => unreachable!(),
        })
    }
}

// States own the checkpoint. Only successful completion can produce the next
// state; no caller can manufacture evidence of a persistence barrier.
struct Writes {
    checkpoint: Checkpoint,
    slot: usize,
    jobs: VecDeque<Job>,
    active: VecDeque<IoTicket>,
}
struct DataSync {
    checkpoint: Checkpoint,
    slot: usize,
    ticket: Option<IoTicket>,
}
struct DataSynced {
    checkpoint: Checkpoint,
    slot: usize,
    ticket: Option<IoTicket>,
}
struct MagicWritten {
    checkpoint: Checkpoint,
    slot: usize,
    ticket: Option<IoTicket>,
}
enum Pipeline {
    Writes(Writes),
    DataSync(DataSync),
    DataSynced(DataSynced),
    MagicWritten(MagicWritten),
}

fn freeze(node: &mut Rc<Node>, space: &Rc<Space>, jobs: &mut VecDeque<Job>) -> io::Result<()> {
    if node.disk.is_some() || node.len() == 0 {
        return Ok(());
    }
    let node = Rc::make_mut(node);
    match &mut node.body {
        Body::Leaf(values) => {
            for (_, value) in values {
                if let Entry::Payload(value) = value
                    && !value.written.get()
                {
                    jobs.push_back(Job::Value(value.clone()));
                }
            }
        }
        Body::Branch(children) => {
            for child in children {
                freeze(child, space, jobs)?;
            }
        }
    }
    let allocation = space.allocate(Class::Index)?;
    jobs.push_back(Job::Page(encode(node), allocation.offset()));
    node.disk = Some(allocation);
    Ok(())
}
// Visit changed paths only. Splits/merges may visit their small neighboring
// subtrees; unchanged disk nodes are shared and terminate traversal immediately.
fn changed_bits(node: &Node, other: Option<&Node>, used: &mut [u8], occupied: bool) {
    if let (Some(disk), Some(other)) = (&node.disk, other)
        && other.disk.as_ref().is_some_and(|a| a.page() == disk.page())
    {
        return;
    }
    let mut set = |page: usize, count: usize| {
        for i in page..page + count {
            if occupied {
                used[i / 8] |= 1 << (i % 8);
            } else {
                used[i / 8] &= !(1 << (i % 8));
            }
        }
    };
    if let Some(disk) = &node.disk {
        set(disk.page(), 1);
    }
    match &node.body {
        Body::Leaf(values) => {
            for (_, value) in values {
                if let Entry::Payload(value) = value {
                    set(value.allocation.page(), 1024);
                }
            }
        }
        Body::Branch(children) => {
            for child in children {
                let peer = other.and_then(|other| match &other.body {
                    Body::Branch(peers) => peers
                        .binary_search_by_key(&child.first(), |p| p.first())
                        .ok()
                        .map(|i| peers[i].as_ref()),
                    _ => None,
                });
                changed_bits(child, peer, used, occupied);
            }
        }
    }
}
impl Allocator {
    fn prepare(&mut self) -> io::Result<Pipeline> {
        let generation = self
            .generation()
            .checked_add(1)
            .ok_or_else(|| invalid("checkpoint generation exhausted"))?;
        let slot = if self.checkpoints[0].as_ref().map_or(0, |c| c.generation)
            < self.checkpoints[1].as_ref().map_or(0, |c| c.generation)
        {
            0
        } else {
            1
        };
        let mut root = self.root.clone();
        let mut jobs = VecDeque::new();
        freeze(&mut root, &self.space, &mut jobs)?;
        let mut used = vec![0; self.space.geometry.pages().div_ceil(8)];
        let latest = self
            .checkpoints
            .iter()
            .flatten()
            .max_by_key(|c| c.generation)
            .unwrap();
        for (chunk, (_, bitmap)) in used.chunks_mut(BIT_BYTES).zip(&latest.bitmaps) {
            chunk.copy_from_slice(&bitmap.0[32..32 + chunk.len()]);
        }
        changed_bits(&latest.root, Some(&root), &mut used, false);
        changed_bits(&root, Some(&latest.root), &mut used, true);
        let mut bitmaps = Vec::new();
        for (i, chunk) in used.chunks(BIT_BYTES).enumerate() {
            let mut bitmap = Box::new(uring::Page([0; PAGE_SIZE]));
            put(&mut bitmap, 0, BITS);
            put(&mut bitmap, 16, i as u64);
            bitmap.0[32..32 + chunk.len()].copy_from_slice(chunk);
            seal(&mut bitmap);
            let allocation = if let Some((allocation, old)) = latest.bitmaps.get(i)
                && old.0 == bitmap.0
            {
                allocation.clone()
            } else {
                let allocation = self.space.allocate(Class::Index)?;
                jobs.push_back(Job::Page(
                    Box::new(uring::Page(bitmap.0)),
                    allocation.offset(),
                ));
                allocation
            };
            bitmaps.push((allocation, bitmap));
        }
        // Commit prepared locations to the live tree only after all reservations
        // succeed. Subsequent mutations CoW just the shared in-memory path.
        self.root = root.clone();
        // Any not-yet-submitted values are now owned by this frozen batch.
        self.pending.clear();
        self.changed = false;
        self.rotate = generation < self.reclaim_until;
        Ok(Pipeline::Writes(Writes {
            checkpoint: Checkpoint {
                generation,
                root,
                bitmaps,
            },
            slot,
            jobs,
            active: VecDeque::new(),
        }))
    }
    fn progress(
        &mut self,
        io: &mut impl Storage<Ticket = IoTicket>,
        budget: usize,
    ) -> io::Result<bool> {
        self.healthy()?;
        // Remain poisoned on errors AND unwinding. A partially submitted batch
        // may still publish a magic page; never resume allocation after ambiguity.
        self.failed = true;
        let result = self.progress_inner(io, budget);
        if result.is_ok() {
            self.failed = false;
            if self.is_idle() {
                self.release_capacity(self.charged);
            }
        }
        result
    }
    fn progress_inner(
        &mut self,
        io: &mut impl Storage<Ticket = IoTicket>,
        budget: usize,
    ) -> io::Result<bool> {
        let mut runnable = false;
        for _ in 0..budget.min(self.publishing.len()) {
            let mut ticket = self.publishing.pop_front().unwrap();
            match io.complete(&mut ticket)? {
                None => self.publishing.push_back(ticket),
                Some(result) => {
                    result?;
                    runnable = true;
                }
            }
        }
        // Publication is independent of checkpoint fsyncs. A checkpoint is frozen
        // only after its values are readable, preserving data-before-root order.
        let checkpoint_io = match &self.pipeline {
            Some(Pipeline::Writes(writes)) => writes.active.len(),
            Some(_) => 1,
            None => 0,
        };
        // Obsolete unflushed versions never reach disk. Queue ownership bounds
        // retained buffers, and dropping one cannot affect outstanding requests.
        for _ in 0..budget.min(self.pending.len()) {
            let (key, weak) = self.pending.pop_front().unwrap();
            if let Some(value) = weak.upgrade()
                && !value.written.get()
                && self
                    .root
                    .get(&key)
                    .and_then(Entry::payload)
                    .is_some_and(|live| Rc::ptr_eq(live, &value))
            {
                if self.publishing.len() + checkpoint_io >= self.config.max_io {
                    self.pending.push_back((key, weak));
                    continue;
                }
                match io.submit(Job::Value(value)) {
                    Ok(ticket) => {
                        self.publishing.push_back(ticket);
                        runnable = true;
                    }
                    Err(error) => {
                        self.pending.push_front((key, weak));
                        if error.error.kind() != io::ErrorKind::WouldBlock {
                            return Err(error.error);
                        }
                        break;
                    }
                }
            }
        }
        if self.pipeline.is_none()
            && self.pending.is_empty()
            && self.publishing.is_empty()
            && (self.changed || self.rotate)
        {
            self.pipeline = Some(self.prepare()?);
        }
        let Some(pipeline) = self.pipeline.take() else {
            return Ok(runnable || !self.pending.is_empty());
        };
        let next = match pipeline {
            Pipeline::Writes(mut writes) => {
                runnable |= writes.active.len() > budget;
                for _ in 0..budget.min(writes.active.len()) {
                    let mut ticket = writes.active.pop_front().unwrap();
                    match io.complete(&mut ticket)? {
                        None => writes.active.push_back(ticket),
                        Some(result) => {
                            result?;
                            runnable = true;
                        }
                    }
                }
                for _ in 0..budget {
                    if writes.active.len() + self.publishing.len() >= self.config.max_io {
                        break;
                    }
                    let Some(job) = writes.jobs.pop_front() else {
                        break;
                    };
                    match io.submit(job) {
                        Ok(ticket) => {
                            writes.active.push_back(ticket);
                            runnable = true;
                        }
                        Err(e) => {
                            writes.jobs.push_front(e.resource);
                            if e.error.kind() != io::ErrorKind::WouldBlock {
                                return Err(e.error);
                            }
                            break;
                        }
                    }
                }
                if writes.jobs.is_empty() && writes.active.is_empty() {
                    #[cfg(test)]
                    if let Some(world) = crate::simulation::current() {
                        world.event("checkpoint-data-written", "", "awaiting-data-sync");
                    }
                    runnable = true;
                    Some(Pipeline::DataSync(DataSync {
                        checkpoint: writes.checkpoint,
                        slot: writes.slot,
                        ticket: None,
                    }))
                } else {
                    Some(Pipeline::Writes(writes))
                }
            }
            Pipeline::DataSync(mut sync) => {
                if advance(io, &mut sync.ticket, || Job::Sync)? {
                    runnable = true;
                    Some(Pipeline::DataSynced(DataSynced {
                        checkpoint: sync.checkpoint,
                        slot: sync.slot,
                        ticket: None,
                    }))
                } else {
                    Some(Pipeline::DataSync(sync))
                }
            }
            Pipeline::DataSynced(mut sync) => {
                if advance(io, &mut sync.ticket, || {
                    let c = &sync.checkpoint;
                    let bitmaps: Vec<_> = c.bitmaps.iter().map(|(a, _)| a.clone()).collect();
                    Job::Page(
                        magic(
                            self.space.geometry,
                            c.generation,
                            c.root.disk.as_ref().map_or(0, |a| a.page() as u64),
                            &bitmaps,
                        ),
                        self.space.geometry.offset(sync.slot),
                    )
                })? {
                    runnable = true;
                    Some(Pipeline::MagicWritten(MagicWritten {
                        checkpoint: sync.checkpoint,
                        slot: sync.slot,
                        ticket: None,
                    }))
                } else {
                    Some(Pipeline::DataSynced(sync))
                }
            }
            Pipeline::MagicWritten(mut written) => {
                if advance(io, &mut written.ticket, || Job::Sync)? {
                    self.checkpoints[written.slot] = Some(written.checkpoint);
                    runnable = true;
                    None
                } else {
                    Some(Pipeline::MagicWritten(written))
                }
            }
        };
        self.pipeline = next;
        Ok(runnable)
    }
}
fn advance(
    io: &mut impl Storage<Ticket = IoTicket>,
    ticket: &mut Option<IoTicket>,
    job: impl FnOnce() -> Job,
) -> io::Result<bool> {
    if let Some(ticket) = ticket {
        return match io.complete(ticket)? {
            Some(result) => result.map(|()| true),
            None => Ok(false),
        };
    }
    match io.submit(job()) {
        Ok(submitted) => *ticket = Some(submitted),
        Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => {}
        Err(e) => return Err(e.error),
    }
    Ok(false)
}

#[cfg(test)]
#[path = "../tests/storage/recovery.rs"]
mod model_tests;
#[cfg(test)]
#[path = "../tests/storage/allocator.rs"]
mod tests;
