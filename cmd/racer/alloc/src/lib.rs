//! Worker-local aligned storage with caller-owned accounting and direct I/O.
//!
//! This internal crate owns generic bytes, the [`volume`] extent store, and
//! small retention filters in [`retention`]. It does not own record formats,
//! encryption, integrity checks, admission classes, or key schemas. The caller
//! supplies accounting guards, a reactor, and cancellation policy. There is no
//! assumed OS page size and no cross-thread allocation authority.
//!
//! # Startup and binding
//!
//! Construct [`Slab::new`] with a full file path, or [`Slab::from_devices`]
//! with one [`DevicePlacement`] per segment, then call [`Slab::open_volume`]
//! outside the worker's latency-sensitive path. Directory traversal, locking,
//! sizing, and probing are blocking. Alignment comes from
//! `statx(STATX_DIOALIGN)` rather than a fixed page size. The maximum record
//! size only checks that its padded size fits a segment at startup.
//!
//! Linux traversal rejects parent components and intermediate/final symlinks.
//! Missing directories and files are created with modes 0700 and 0600, subject to
//! umask. Accepted files must be regular, owned by the effective user, singly
//! linked, and exactly mode 0600 with no special bits. Permissions are not repaired.
//! Nonblocking open prevents FIFOs from hanging startup before type validation.
//! Files are close-on-exec, exclusively locked with nonblocking flock, and use
//! direct I/O. Parent directories still must be trusted against hostile rename
//! and unlink; neither traversal nor flock stops noncooperating writers.
//!
//! In device mode the caller opens devices with O_EXCL and O_DIRECT and supplies
//! common alignment. Bounds use BLKGETSIZE64 for block devices and length for
//! regular files. Placements can share an `Arc<File>` to use one runtime
//! descriptor per device. Startup never creates, resizes, or locks these files.
//! Offsets are logical slab offsets; submissions translate them to the
//! placement's physical range. Overlap checks only compare the same inode or
//! device identity within one slab. They cannot detect whole-disk, partition, or
//! device-mapper aliases. The caller must guarantee disjoint physical storage.
//!
//! In file mode, empty files are sparsely extended to capacity and synced with
//! their parent directories; nonempty size mismatches are rejected without
//! truncation. Capacity is a logical bound, not reserved disk space, so later
//! writes can fail with ENOSPC. Buffer zeroization is not secure erasure of the
//! file.
//!
//! # Alignment and accounting
//!
//! [`Alignment`] permits arbitrary positive offset and length units; memory
//! alignment must be a supported power of two. Extent padding uses their least
//! common multiple, so offset unit 768 and length unit 512 require multiples of
//! 1536. Arithmetic and the conservative 1 GiB transfer cap are checked before
//! allocating. Larger logical records must be split by the caller.
//!
//! [`AlignedBuffer`] owns initialized stable storage that cannot be resized.
//! The primary [`Charge`] must truthfully account for its entire padded size;
//! `()` opts out of accounting. Additional guards attached with
//! [`AlignedBuffer::retain`] remain live through completion, but not idle pooling.
//! [`Slab::allocate`] reuses a bounded free list: at most 8 idle buffers and
//! 128 MiB, matched by 64 KiB size class. Idle memory retains its primary
//! charge. Reuse requires admission for the full capacity and replaces the old
//! charge. Full, borrowed, or dropped pools do not retain returned storage.
//! [`Slab::reclaim_idle`] frees idle bytes only, never memory owned by the
//! kernel. [`AlignedBuffer::freeze`] detaches completed storage as a sendable,
//! immutable [`FrozenBuffer`].
//!
//! # Completion ownership
//!
//! [`Slab::read_at`] and [`Slab::write_at`] check alignment and that the
//! transfer stays inside one bound segment. Accepted I/O owns the buffer, all
//! charges, and the caller's guard through the runtime completion fence, even
//! when its waiting future is dropped. Short completions fail. An unpolled
//! future has submitted nothing. [`Slab::fence_writes`] waits for accepted
//! writes to complete; it is not a durability barrier. [`Slab::sync`] fsyncs
//! every backing descriptor.
//!
//! # Validation
//!
//! Run `cargo test -p page-alloc --no-default-features` and
//! `cargo test -p page-alloc --all-features` under an external TERM timeout.
//! Simulation provides deterministic fault and cancellation ordering, not proof
//! of filesystem security or kernel support. Real tests print explicit
//! capability skips with `-- --nocapture`; set `PAGE_ALLOC_REQUIRE_REAL_IO=1`
//! to turn those skips into failures. Miri can cover the pure `buffer_tests`.
#![deny(unsafe_op_in_unsafe_fn)]
#![deny(missing_docs)]

mod demand;
#[cfg(feature = "simulation")]
pub mod index;
pub mod retention;
// Disk model for racer-sim only; the dataplane uses `volume`.
#[cfg(feature = "simulation")]
mod segments;
mod slab;
pub mod volume;

pub use demand::{
    DEFAULT_DECAY_WINDOWS, DEFAULT_INITIAL_TARGET, DEFAULT_MAX_DEPTH, DEFAULT_MIN_DEPTH,
    DEFAULT_WINDOW, PoolDemand, PoolStats,
};
#[cfg(feature = "simulation")]
pub use segments::{
    FreezeGuard, Generation, MAX_SEGMENTS, PhantomEntries, SegmentClock, SegmentEntries,
    SegmentGeometry, SegmentId, SegmentLease, SegmentSnapshot, SegmentState, Segments,
};
pub use slab::{DevicePlacement, Slab};
use std::{
    alloc::{Layout, alloc_zeroed, dealloc},
    cell::RefCell,
    collections::VecDeque,
    fmt,
    mem::ManuallyDrop,
    ptr::NonNull,
    rc::{Rc, Weak},
    sync::{Arc, Mutex, MutexGuard, Weak as SyncWeak},
};
use uring_runtime::reactor::IoBuffer;

/// Storage failures, separated from application record and admission policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
pub enum Error {
    /// The filesystem or kernel cannot provide required direct-I/O support.
    Unsupported,

    /// Caller configuration or an implementation contract is invalid.
    InvalidConfiguration,

    /// A transient lease or resource limit prevents progress.
    Busy,

    /// An extent or persisted allocator image is malformed.
    Corrupt,

    /// A generation or binding is no longer valid.
    Stale,

    /// Storage is unopened, locked elsewhere, or has exhausted a generation.
    Unavailable,

    /// An I/O completion was short or failed without OS error detail.
    Io,

    /// Synchronous OS failure; asynchronous errors retain the runtime's error type.
    SystemIo {
        /// Operation that failed.
        operation: &'static str,

        /// Operating-system error number when available.
        errno: Option<i32>,
    },
}

impl fmt::Display for Error {
    /// Describe the category, preserving operating-system diagnostics when present.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if let Self::SystemIo { operation, errno } = self {
            return match errno {
                Some(errno) => write!(
                    f,
                    "{operation}: {} (errno {errno})",
                    std::io::Error::from_raw_os_error(*errno)
                ),
                None => write!(f, "{operation}: storage I/O failed"),
            };
        }
        f.write_str(match self {
            Self::Unsupported => "direct I/O is unsupported",
            Self::InvalidConfiguration => "invalid storage configuration",
            Self::Busy => "storage is busy or exhausted",
            Self::Corrupt => "invalid storage extent or generation",
            Self::Stale => "stale storage generation, state, or table",
            Self::Unavailable => "storage is unavailable",
            Self::Io => "storage I/O failed",
            Self::SystemIo { .. } => unreachable!(),
        })
    }
}

impl std::error::Error for Error {}

/// Result of an allocator operation before conversion into a caller's scope error.
pub type Result<T> = std::result::Result<T, Error>;

/// Caller-owned accounting retained while memory is live, including idle pooling.
pub trait Charge: 'static {
    /// Whether this guard accounts for at least `bytes` live allocation bytes.
    fn covers(&self, bytes: usize) -> bool;

    /// Secrets must be erased before storage or accounting is released.
    fn requires_wipe(&self) -> bool {
        true
    }
}

impl Charge for () {
    /// Explicitly opt out of accounting for callers without admission policy.
    fn covers(&self, _bytes: usize) -> bool {
        true
    }
}

/// Direct-I/O alignment requirements. Offset and length units may be any positive
/// integers; only the memory alignment must be a power of two.
#[must_use]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Alignment {
    memory: usize,

    offset: u64,

    length: usize,
}

/// A nonempty, non-overflowing file range for one bounded I/O transfer.
#[must_use]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Extent {
    offset: u64,

    length: usize,
}

impl Extent {
    /// Reject empty ranges, offset overflow, and lengths above the transfer cap.
    pub fn new(offset: u64, length: usize) -> Result<Self> {
        if length == 0
            || length > Alignment::MAX_TRANSFER_LENGTH
            || offset.checked_add(length as u64).is_none()
        {
            return Err(Error::Corrupt);
        }
        Ok(Self { offset, length })
    }

    /// Starting byte offset in the file.
    #[must_use]
    pub fn offset(self) -> u64 {
        self.offset
    }

    /// Transfer length in bytes, including any padding.
    #[must_use]
    pub fn length(self) -> usize {
        self.length
    }
}

impl Alignment {
    /// Conservative single-transfer limit of 1 GiB.
    ///
    /// This fits the runtime's `u32` length and stays below Linux's
    /// `MAX_RW_COUNT` (`i32::MAX` rounded down to a base-page boundary) on
    /// supported Linux base-page sizes, without querying host state. Keeping a
    /// fixed conservative bound also makes geometry checks deterministic under
    /// simulation and Miri. Larger records must be split by the caller.
    pub const MAX_TRANSFER_LENGTH: usize = 1 << 30;

    /// Validate alignment units without requiring offset/length powers of two.
    pub fn new(memory: usize, offset: u64, length: usize) -> Result<Self> {
        if !memory.is_power_of_two() || offset == 0 || length == 0 || memory > isize::MAX as usize {
            return Err(Error::Unsupported);
        }
        Ok(Self {
            memory,
            offset,
            length,
        })
    }

    /// Required memory address alignment.
    #[must_use]
    pub fn memory(self) -> usize {
        self.memory
    }

    /// Required file offset unit.
    #[must_use]
    pub fn offset(self) -> u64 {
        self.offset
    }

    /// Required transfer length unit.
    #[must_use]
    pub fn length(self) -> usize {
        self.length
    }

    /// Round up to the least common multiple of offset and length units so the
    /// next appended extent is also offset-aligned. Reject overflow and lengths
    /// above [`Self::MAX_TRANSFER_LENGTH`] before allocating memory.
    pub fn extent(&self, offset: u64, logical: usize) -> Result<Extent> {
        if !offset.is_multiple_of(self.offset)
            || logical == 0
            || logical > Self::MAX_TRANSFER_LENGTH
        {
            return Err(Error::InvalidConfiguration);
        }
        let offset_unit = usize::try_from(self.offset).map_err(|_| Error::InvalidConfiguration)?;
        let (mut a, mut b) = (offset_unit, self.length);
        while b != 0 {
            (a, b) = (b, a % b);
        }
        let unit = (offset_unit / a)
            .checked_mul(self.length)
            .ok_or(Error::InvalidConfiguration)?;
        let length = logical
            .checked_add(unit - 1)
            .and_then(|v| (v / unit).checked_mul(unit))
            .ok_or(Error::InvalidConfiguration)?;
        if length > Self::MAX_TRANSFER_LENGTH {
            return Err(Error::InvalidConfiguration);
        }
        Extent::new(offset, length)
    }

    /// Allocate zeroed stable storage and retain its primary accounting guard.
    /// Length must be nonzero, length-aligned, covered by `charge`, and no larger
    /// than [`Self::MAX_TRANSFER_LENGTH`]. Allocation failure returns `Busy`.
    pub fn allocate<C: Charge>(&self, length: usize, charge: C) -> Result<AlignedBuffer<C>> {
        if length == 0
            || length > Self::MAX_TRANSFER_LENGTH
            || !length.is_multiple_of(self.length)
            || !charge.covers(length)
        {
            return Err(Error::InvalidConfiguration);
        }
        let layout = Layout::from_size_align(length, self.memory)
            .map_err(|_| Error::InvalidConfiguration)?;
        let (pointer, release) = allocate_zeroed(layout)?;
        Ok(AlignedBuffer {
            allocation: Some(Allocation {
                pointer,
                layout,
                release,
                charge,
                retained: Vec::new(),
                clean: true,
                #[cfg(test)]
                wipes: WipeCount::default(),
            }),
            pool: Weak::new(),
            length,
        })
    }

    /// Validate the address, file offset, transfer unit, and exact buffer length.
    pub fn check<C: Charge>(&self, extent: Extent, buffer: &AlignedBuffer<C>) -> Result<()> {
        if !(buffer.allocation().pointer.as_ptr() as usize).is_multiple_of(self.memory)
            || !extent.offset.is_multiple_of(self.offset)
            || !extent.length.is_multiple_of(self.length)
            || buffer.len() != extent.length
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

const HUGE_PAGE_BYTES: usize = 2 * 1024 * 1024;

/// Remember the allocator, since mmap storage must never reach System::dealloc.
enum Release {
    Heap,
    #[cfg(all(target_os = "linux", not(miri)))]
    Mapping(usize),
}

/// Keep all publicly exposed bytes initialized, including on allocation failure.
fn allocate_zeroed(layout: Layout) -> Result<(NonNull<u8>, Release)> {
    #[cfg(all(target_os = "linux", not(miri)))]
    if layout.size() >= HUGE_PAGE_BYTES {
        // SAFETY: sysconf has no borrowed memory. Reject an invalid page size.
        let page = usize::try_from(unsafe { libc::sysconf(libc::_SC_PAGESIZE) })
            .ok()
            .filter(|n| n.is_power_of_two())
            .ok_or(Error::Busy)?;
        let alignment = layout.align().max(HUGE_PAGE_BYTES).max(page);
        let length = layout
            .size()
            .checked_add(page - 1)
            .map(|n| n / page * page)
            .ok_or(Error::Busy)?;
        let reserve = length
            .checked_add(alignment)
            .filter(|n| *n <= isize::MAX as usize)
            .ok_or(Error::Busy)?;
        // SAFETY: anonymous private mmap returns zeroed, writable, exclusively
        // owned pages or MAP_FAILED. Overmapping allows alignment to both the
        // runtime DIO requirement and 2 MiB without MAP_FIXED. Prefix and suffix
        // are page multiples and are unmapped before exposing the middle. Only
        // layout.size() initialized bytes are exposed. Release::Mapping records
        // the remaining page-rounded length for exactly one matching munmap.
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                reserve,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return Err(Error::Busy);
        }
        if base.is_null() {
            // Rust slices require nonnull pointers even if address zero mapped.
            unsafe {
                libc::munmap(base, reserve);
            }
            return Err(Error::Busy);
        }
        let address = base as usize;
        let prefix = address.wrapping_neg() & (alignment - 1);
        // SAFETY: prefix is inside this mapping; all trimmed ranges are page-aligned.
        let pointer = unsafe { base.cast::<u8>().add(prefix) };
        let suffix = reserve - prefix - length;
        unsafe {
            if prefix != 0 && libc::munmap(base, prefix) != 0 {
                libc::munmap(base, reserve);
                return Err(Error::Busy);
            }
            if suffix != 0 && libc::munmap(pointer.add(length).cast(), suffix) != 0 {
                libc::munmap(pointer.cast(), reserve - prefix);
                return Err(Error::Busy);
            }
        }
        let Some(pointer) = NonNull::new(pointer) else {
            unsafe {
                libc::munmap(pointer.cast(), length);
            }
            return Err(Error::Busy);
        };
        // SAFETY: advisory only on our live mapping. Unsupported THP is harmless.
        unsafe {
            libc::madvise(pointer.as_ptr().cast(), length, libc::MADV_HUGEPAGE);
        }
        return Ok((pointer, Release::Mapping(length)));
    }
    // SAFETY: valid nonzero layout; the owner records matching heap deallocation.
    NonNull::new(unsafe { alloc_zeroed(layout) })
        .map(|pointer| (pointer, Release::Heap))
        .ok_or(Error::Busy)
}

/// Owns raw storage and accounting together; moving it transfers both exactly once.
struct Allocation<C: Charge> {
    pointer: NonNull<u8>,

    layout: Layout,

    release: Release,

    charge: C,

    retained: Vec<Rc<C>>,

    // True only after zeroed allocation, discard, or complete clearing. Every mutable
    // exposure, including the runtime's kernel-write borrow, clears this first.
    clean: bool,

    #[cfg(test)]
    wipes: WipeCount,
}

/// Test-only wipe counter that stays sound when storage crosses threads.
#[cfg(test)]
#[derive(Clone, Default)]
struct WipeCount(Arc<std::sync::atomic::AtomicUsize>);

#[cfg(test)]
impl WipeCount {
    fn get(&self) -> usize {
        self.0.load(std::sync::atomic::Ordering::SeqCst)
    }

    fn increment(&self) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}

impl<C: Charge> Allocation<C> {
    /// Ciphertext mappings can discard pages; secrets retain secure erasure.
    fn prepare_pool(&mut self) {
        if self.clean {
            return;
        }
        if self.charge.requires_wipe() {
            self.wipe();
            return;
        }
        #[cfg(all(target_os = "linux", not(miri)))]
        if let Release::Mapping(length) = self.release {
            // SAFETY: exclusive anonymous private mapping, no outstanding I/O.
            // DONTNEED replaces discarded contents with demand-zero pages.
            if unsafe { libc::madvise(self.pointer.as_ptr().cast(), length, libc::MADV_DONTNEED) }
                == 0
            {
                self.clean = true;
                return;
            }
        }
        self.as_mut_slice().fill(0);
        self.clean = true;
    }

    /// Exclusively borrow the complete initialized allocation with its original size.
    fn as_mut_slice(&mut self) -> &mut [u8] {
        self.clean = false;
        // SAFETY: exclusive owner, initialized nonzero allocation, original layout.
        unsafe { std::slice::from_raw_parts_mut(self.pointer.as_ptr(), self.layout.size()) }
    }

    /// Securely erase the entire allocation once after each mutable exposure.
    fn wipe(&mut self) {
        if self.clean {
            return;
        }
        #[cfg(all(
            target_os = "linux",
            any(target_env = "gnu", target_env = "musl"),
            not(miri)
        ))]
        // SAFETY: this exclusive owner holds layout.size() initialized writable
        // bytes. explicit_bzero cannot be eliminated as a dead store. No kernel
        // operation can outlive ownership's completion fence.
        unsafe {
            libc::explicit_bzero(self.pointer.as_ptr().cast(), self.layout.size())
        };
        #[cfg(not(all(
            target_os = "linux",
            any(target_env = "gnu", target_env = "musl"),
            not(miri)
        )))]
        {
            use zeroize::Zeroize;
            self.as_mut_slice().zeroize();
        }
        self.clean = true;
        #[cfg(test)]
        self.wipes.increment();
    }
}

impl<C: Charge> Drop for Allocation<C> {
    /// Erase and free before field destruction releases the accounting guards.
    fn drop(&mut self) {
        if self.charge.requires_wipe() {
            self.wipe();
        }
        // SAFETY: this owner holds the allocation and its original release path.
        // Guards drop after required secret erasure and deallocation complete.
        match self.release {
            Release::Heap => unsafe { dealloc(self.pointer.as_ptr(), self.layout) },
            #[cfg(all(target_os = "linux", not(miri)))]
            Release::Mapping(length) => unsafe {
                libc::munmap(self.pointer.as_ptr().cast(), length);
            },
        }
    }
}

/// Worker-local, exclusively owned, initialized storage with a stable address.
///
/// Moving the buffer never moves its bytes. Drop securely erases secrets before
/// freeing or pooling them. Idle storage retains its primary charge, but not
/// additional guards attached with [`Self::retain`].
///
/// A buffer cannot cross a worker boundary even when its accounting guard can:
///
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<page_alloc::AlignedBuffer<()>>();
/// ```
#[must_use]
pub struct AlignedBuffer<C: Charge> {
    // Always Some while publicly accessible; taken only by Drop for pool transfer.
    allocation: Option<Allocation<C>>,

    pool: Weak<RefCell<BufferPool<C>>>,

    length: usize,
}

/// Small demand-sized free lists, grouped in 64 KiB size classes without locks.
///
/// Entries are kept oldest first so maintenance trims the longest-idle storage.
pub(crate) struct BufferPool<C: Charge> {
    entries: VecDeque<AlignedBuffer<C>>,
    demand: PoolDemand,
    budget: usize,
}

impl<C: Charge> Default for BufferPool<C> {
    fn default() -> Self {
        Self {
            entries: VecDeque::new(),
            demand: PoolDemand::new(),
            budget: 128 * 1024 * 1024,
        }
    }
}

/// Round backing capacity without changing the length of an I/O transfer.
pub fn buffer_capacity(length: usize) -> Option<usize> {
    const UNIT: usize = 64 * 1024;
    length.checked_add(UNIT - 1).map(|n| n / UNIT * UNIT)
}

impl<C: Charge> BufferPool<C> {
    fn bytes(&self) -> usize {
        self.entries
            .iter()
            .map(|b| b.allocation().layout.size())
            .sum()
    }

    fn accepts(&mut self, capacity: usize) -> bool {
        let bytes = self.bytes();
        self.demand
            .admit(self.entries.len(), bytes, capacity, self.budget)
    }

    /// Remove the entries chosen by demand maintenance, oldest first.
    pub(crate) fn maintain(&mut self, now: std::time::Instant) -> Vec<AlignedBuffer<C>> {
        let release = self.demand.tick(now, self.entries.len());
        self.entries.drain(..release).collect()
    }

    /// Remove every idle entry and count it as trimmed.
    pub(crate) fn take_all(&mut self) -> VecDeque<AlignedBuffer<C>> {
        let idle = std::mem::take(&mut self.entries);
        self.demand.record_trim(idle.len(), 0);
        idle
    }

    pub(crate) fn set_constrained(&mut self, constrained: bool) {
        self.demand.set_constrained(constrained);
    }

    pub(crate) fn stats(&self) -> PoolStats {
        self.demand.stats(self.entries.len(), self.bytes())
    }

    fn take_for(&mut self, length: usize, charge: &C) -> Option<AlignedBuffer<C>> {
        let class = buffer_capacity(length)?;
        let index = self.entries.iter().position(|b| {
            let capacity = b.allocation().layout.size();
            buffer_capacity(capacity) == Some(class)
                && capacity >= length
                && charge.covers(capacity)
        });
        let Some(index) = index else {
            self.demand.record_miss();
            return None;
        };
        let buffer = self.entries.remove(index);
        self.demand.record_hit(self.entries.len());
        buffer
    }
}

impl<C: Charge> fmt::Debug for AlignedBuffer<C> {
    /// Show storage properties without requiring accounting guards to expose data.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("AlignedBuffer")
            .field("length", &self.len())
            .field("alignment", &self.allocation().layout.align())
            .field("retained", &self.allocation().retained.len())
            .finish_non_exhaustive()
    }
}

impl<C: Charge> AlignedBuffer<C> {
    /// Freeze completed I/O storage without copying or retaining a worker-local pool.
    pub fn freeze(mut self) -> Result<FrozenBuffer<C>> {
        if !self.allocation().retained.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        // Test-only counters must not keep an Rc alias on the source thread.
        #[cfg(test)]
        {
            self.allocation_mut().wipes = WipeCount::default();
        }
        Ok(FrozenBuffer {
            allocation: ManuallyDrop::new(self.allocation.take().expect("live buffer allocation")),
            length: self.length,
            padded: self.length,
            origin: SyncWeak::new(),
        })
    }

    /// Borrow the allocation, which is absent only during drop's ownership transfer.
    fn allocation(&self) -> &Allocation<C> {
        self.allocation.as_ref().expect("live buffer allocation")
    }

    /// Mutably borrow live backing storage before any drop-time pool transfer.
    fn allocation_mut(&mut self) -> &mut Allocation<C> {
        self.allocation.as_mut().expect("live buffer allocation")
    }

    /// Attach a weak return destination without extending the pool's lifetime.
    pub(crate) fn pooled(mut self, pool: &Rc<RefCell<BufferPool<C>>>) -> Self {
        self.pool = Rc::downgrade(pool);
        self
    }

    /// Replace primary accounting only after the new guard covers the full size.
    pub(crate) fn rebind(&mut self, charge: C) -> Result<()> {
        if !charge.covers(self.allocation().layout.size()) {
            return Err(Error::InvalidConfiguration);
        }
        self.allocation_mut().charge = charge;
        Ok(())
    }

    /// Retain additional accounting through the final kernel completion.
    pub fn retain(&mut self, charge: Rc<C>) {
        self.allocation_mut().retained.push(charge);
    }

    /// Initialized allocation length, including padding.
    #[must_use]
    pub fn len(&self) -> usize {
        self.length
    }

    /// Always false: construction rejects zero-length buffers.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        false
    }

    /// Borrow all initialized bytes without moving or resizing the allocation.
    #[must_use]
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: initialized allocation remains live throughout this borrow.
        unsafe { std::slice::from_raw_parts(self.allocation().pointer.as_ptr(), self.len()) }
    }

    /// Exclusively borrow all initialized bytes.
    #[must_use]
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        let length = self.length;
        &mut self.allocation_mut().as_mut_slice()[..length]
    }

    /// Compatibility accessor matching [`IoBuffer`]; this always succeeds.
    pub fn bytes(&self) -> Result<&[u8]> {
        Ok(self.as_slice())
    }

    /// Compatibility accessor matching [`IoBuffer`]; this always succeeds.
    pub fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(self.as_mut_slice())
    }
}

impl<C: Charge> Drop for AlignedBuffer<C> {
    /// Zeroize and return storage if possible, otherwise let its owner free it.
    fn drop(&mut self) {
        if self.allocation.is_none() {
            return;
        }
        if let Some(pool) = self.pool.upgrade() {
            if self.allocation().charge.requires_wipe() {
                self.allocation_mut().wipe();
            }
            // Caller-owned guard destructors may access the pool. Do not invoke
            // them while holding its RefCell borrow. Allocation remains owned
            // by this buffer if a destructor unwinds.
            self.allocation_mut().retained.clear();
            if let Ok(mut idle) = pool.try_borrow_mut()
                && idle.accepts(self.allocation().layout.size())
            {
                self.allocation_mut().prepare_pool();
                idle.entries.push_back(Self {
                    allocation: self.allocation.take(),
                    pool: Weak::new(),
                    length: self.length,
                });
            }
        }
        // Field drop securely wipes any still-dirty allocation and frees it. A
        // rejected pool return or idle free needs no second wipe. Idle buffers
        // never repool themselves.
    }
}

/// Immutable allocation detached from the worker-local pool after I/O completion.
///
/// A buffer frozen from a [`DetachedBuffer`] returns its storage to the
/// originating [`WorkerBufferPool`] when dropped, on whichever thread drops it.
/// A buffer frozen from an [`AlignedBuffer`] is simply freed.
pub struct FrozenBuffer<C: Charge> {
    // Taken only by Drop.
    allocation: ManuallyDrop<Allocation<C>>,
    length: usize,
    padded: usize,
    origin: SyncWeak<SharedPool<C>>,
}

// SAFETY: freeze rejects retained Rc guards and removes the worker-local pool
// link; detached buffers never hold retained guards. The owned allocation has
// no mutable byte API; only its charge can change exclusively. Moving or
// sharing it cannot move the bytes or race with I/O or pool access. The origin
// link is a sync::Weak to a Mutex-guarded pool that is Send + Sync for C: Send.
unsafe impl<C: Charge + Send> Send for FrozenBuffer<C> {}
// SAFETY: shared access exposes only immutable bytes and a Sync charge.
unsafe impl<C: Charge + Sync> Sync for FrozenBuffer<C> {}

impl<C: Charge> Drop for FrozenBuffer<C> {
    /// Return pooled storage to its origin, otherwise wipe if required and free.
    fn drop(&mut self) {
        // SAFETY: Drop runs once and the allocation is not used afterward.
        let allocation = unsafe { ManuallyDrop::take(&mut self.allocation) };
        give_back(&self.origin, allocation);
    }
}

impl<C: Charge> FrozenBuffer<C> {
    /// Borrow the initialized I/O extent, excluding spare allocation capacity.
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: immutable initialized backing remains owned for this borrow.
        unsafe { std::slice::from_raw_parts(self.allocation.pointer.as_ptr(), self.length) }
    }

    /// Borrow the extent rounded up to the direct-I/O length unit.
    ///
    /// For buffers frozen from a [`DetachedBuffer`] the bytes past
    /// [`Self::as_slice`] are zero. For buffers frozen from an
    /// [`AlignedBuffer`], whose length is already unit-aligned, this equals
    /// [`Self::as_slice`] and carries no additional zeroing guarantee.
    pub fn padded_slice(&self) -> &[u8] {
        // SAFETY: padded never exceeds the initialized allocation size.
        unsafe { std::slice::from_raw_parts(self.allocation.pointer.as_ptr(), self.padded) }
    }

    /// Complete allocation capacity that must remain charged.
    pub fn capacity(&self) -> usize {
        self.allocation.layout.size()
    }

    /// Borrow the allocation's accounting owner.
    pub fn charge(&self) -> &C {
        &self.allocation.charge
    }

    /// Replace accounting only when it covers the complete allocation.
    pub fn rebind(&mut self, charge: C) -> Result<()> {
        if !charge.covers(self.capacity()) {
            return Err(Error::InvalidConfiguration);
        }
        self.allocation.charge = charge;
        Ok(())
    }
}

/// Default idle byte budget for a [`WorkerBufferPool`].
pub const DEFAULT_SHARED_POOL_BUDGET: usize = 128 * 1024 * 1024;

/// Maximum idle buffers kept per size class in a [`WorkerBufferPool`].
pub const SHARED_POOL_CLASS_DEPTH: usize = 8;

/// Idle storage parked in a shared pool. Only allocations without retained
/// Rc guards are ever wrapped, so moving them between threads is sound.
struct Idle<C: Charge>(Allocation<C>);

// SAFETY: Idle allocations have empty retained lists (checked on entry), are
// exclusively owned, and have no outstanding I/O. The charge moves with it.
unsafe impl<C: Charge + Send> Send for Idle<C> {}

/// Idle entries and demand state for one size class.
struct SharedClass<C: Charge> {
    // Most recently returned last.
    entries: Vec<Idle<C>>,
    demand: PoolDemand,
}

impl<C: Charge> SharedClass<C> {
    fn new(constrained: bool) -> Self {
        let mut demand = PoolDemand::new()
            .with_bounds(0, SHARED_POOL_CLASS_DEPTH)
            .with_target(SHARED_POOL_CLASS_DEPTH);
        demand.set_constrained(constrained);
        Self {
            entries: Vec::new(),
            demand,
        }
    }
}

struct SharedPoolState<C: Charge> {
    // Keyed by buffer_capacity size class. Each class sizes, admits, tracks
    // hits, and trims against its own depth, so mixed-size reuse in one class
    // never makes another class look idle.
    classes: std::collections::BTreeMap<usize, SharedClass<C>>,
    bytes: usize,
    count: usize,
    constrained: bool,
    // Cumulative counters of classes dropped after decaying to empty.
    retired: PoolStats,
}

impl<C: Charge> SharedPoolState<C> {
    fn class(&mut self, capacity: usize) -> &mut SharedClass<C> {
        let constrained = self.constrained;
        self.classes
            .entry(capacity)
            .or_insert_with(|| SharedClass::new(constrained))
    }
}

/// Shared state behind [`WorkerBufferPool`] handles and origin links.
struct SharedPool<C: Charge> {
    alignment: Alignment,
    budget: usize,
    state: Mutex<SharedPoolState<C>>,
}

impl<C: Charge> SharedPool<C> {
    /// Never panic on poisoning: the state stays consistent between statements.
    fn lock(&self) -> MutexGuard<'_, SharedPoolState<C>> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

/// Idle-storage counters for a [`WorkerBufferPool`].
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct SharedPoolStats {
    /// Number of idle buffers.
    pub idle_buffers: usize,
    /// Total capacity of idle buffers in bytes.
    pub idle_bytes: usize,
}

/// Return storage to its origin pool, or wipe if required and free it.
///
/// Wiping and every allocation or charge destructor run outside the lock, so a
/// destructor that touches the same pool cannot deadlock.
fn give_back<C: Charge>(origin: &SyncWeak<SharedPool<C>>, mut allocation: Allocation<C>) {
    let Some(pool) = origin.upgrade() else {
        return; // Allocation drop wipes if required and frees.
    };
    if allocation.charge.requires_wipe() {
        allocation.wipe();
    }
    if !allocation.retained.is_empty() {
        return;
    }
    let capacity = allocation.layout.size();
    let rejected = {
        let mut state = pool.lock();
        let room = state
            .bytes
            .checked_add(capacity)
            .is_some_and(|b| b <= pool.budget);
        let (bytes, budget) = (state.bytes, pool.budget);
        // Demand is tracked per size class, like the fixed depth it replaces,
        // so a busy pool keeps today's capacity across classes.
        let class = state.class(capacity);
        let depth = class.entries.len();
        let admitted = room
            && depth < SHARED_POOL_CLASS_DEPTH
            && class.demand.admit(depth, bytes, capacity, budget);
        if admitted {
            class.entries.push(Idle(allocation));
            state.bytes += capacity;
            state.count += 1;
            None
        } else {
            if !room || depth >= SHARED_POOL_CLASS_DEPTH {
                class.demand.record_reject();
            }
            Some(allocation)
        }
    };
    drop(rejected);
}

/// Cheap-to-clone, thread-safe pool of direct-I/O-aligned storage owned by one
/// worker.
///
/// Buffers taken from the pool may be filled and frozen on any thread and
/// always return to this pool when dropped, preserving NUMA locality. Idle
/// buffers keep their previous charge until the next checkout replaces it.
/// The pool keeps at most [`SHARED_POOL_CLASS_DEPTH`] idle buffers per 64 KiB
/// size class and at most `budget` idle bytes; excess storage is freed.
///
/// Each size class has its own [`PoolDemand`] controller sizing its depth. It
/// starts at [`SHARED_POOL_CLASS_DEPTH`], so a fresh or busy class retains as
/// much as a fixed-depth pool would. [`Self::maintain`] releases entries that
/// stayed idle in their class for a whole window, oldest return first, and a
/// class target decays after sustained miss-free windows, so an idle class
/// eventually empties.
pub struct WorkerBufferPool<C: Charge> {
    inner: Arc<SharedPool<C>>,
}

impl<C: Charge> Clone for WorkerBufferPool<C> {
    fn clone(&self) -> Self {
        Self {
            inner: Arc::clone(&self.inner),
        }
    }
}

impl<C: Charge> fmt::Debug for WorkerBufferPool<C> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WorkerBufferPool")
            .field("alignment", &self.inner.alignment)
            .field("budget", &self.inner.budget)
            .field("stats", &self.stats())
            .finish()
    }
}

impl<C: Charge> WorkerBufferPool<C> {
    /// Create a pool with [`DEFAULT_SHARED_POOL_BUDGET`].
    pub fn new(alignment: Alignment) -> Self {
        Self::with_budget(alignment, DEFAULT_SHARED_POOL_BUDGET)
    }

    /// Create a pool keeping at most `budget` idle bytes.
    pub fn with_budget(alignment: Alignment, budget: usize) -> Self {
        Self {
            inner: Arc::new(SharedPool {
                alignment,
                budget,
                state: Mutex::new(SharedPoolState {
                    classes: std::collections::BTreeMap::new(),
                    bytes: 0,
                    count: 0,
                    constrained: false,
                    retired: PoolStats::default(),
                }),
            }),
        }
    }

    /// Alignment applied to every buffer from this pool.
    pub fn alignment(&self) -> Alignment {
        self.inner.alignment
    }

    /// Idle buffer count and bytes.
    #[must_use]
    pub fn stats(&self) -> SharedPoolStats {
        let state = self.inner.lock();
        SharedPoolStats {
            idle_buffers: state.count,
            idle_bytes: state.bytes,
        }
    }

    /// Idle counters combined with demand controller state.
    ///
    /// Counters are summed across size classes. `target` is the largest
    /// per-class target, or [`SHARED_POOL_CLASS_DEPTH`] when no class is
    /// tracked, since a new class starts there; `max_depth` is the per-class
    /// bound.
    #[must_use]
    pub fn pool_stats(&self) -> PoolStats {
        let state = self.inner.lock();
        let mut stats = PoolStats {
            entries: state.count,
            bytes: state.bytes,
            target: 0,
            max_depth: SHARED_POOL_CLASS_DEPTH,
            constrained: state.constrained,
            ..state.retired
        };
        let mut target = None;
        for class in state.classes.values() {
            let c = class.demand.stats(0, 0);
            stats.hits += c.hits;
            stats.misses += c.misses;
            stats.rejects += c.rejects;
            stats.trimmed += c.trimmed;
            target = Some(target.unwrap_or(0).max(c.target));
        }
        stats.target = target.unwrap_or(SHARED_POOL_CLASS_DEPTH);
        stats
    }

    /// Free every idle buffer. Storage is freed after the lock is released.
    pub fn clear(&self) {
        let idle = {
            let mut state = self.inner.lock();
            let mut idle = Vec::with_capacity(state.count);
            for class in state.classes.values_mut() {
                class.demand.record_trim(class.entries.len(), 0);
                idle.append(&mut class.entries);
            }
            state.bytes = 0;
            state.count = 0;
            idle
        };
        drop(idle);
    }

    /// Run demand maintenance at `now` and return the idle bytes released.
    ///
    /// Work is proportional to the number of tracked size classes; at a window
    /// boundary each class controller picks how many of its entries to release
    /// and that class's oldest returns go first. Empty classes whose target
    /// decayed to zero are dropped. Storage is freed after the lock is released.
    pub fn maintain(&self, now: std::time::Instant) -> usize {
        let freed = {
            let mut state = self.inner.lock();
            let mut freed = Vec::new();
            let mut retired = state.retired;
            let mut released_bytes = 0;
            state.classes.retain(|capacity, class| {
                let release = class.demand.tick(now, class.entries.len());
                let release = release.min(class.entries.len());
                freed.extend(class.entries.drain(..release));
                released_bytes += release * capacity;
                let keep = !class.entries.is_empty() || class.demand.target() > 0;
                if !keep {
                    let c = class.demand.stats(0, 0);
                    retired.hits += c.hits;
                    retired.misses += c.misses;
                    retired.rejects += c.rejects;
                    retired.trimmed += c.trimmed;
                }
                keep
            });
            state.retired = retired;
            state.bytes -= released_bytes;
            state.count -= freed.len();
            freed
        };
        let bytes = freed.iter().map(|Idle(a)| a.layout.size()).sum();
        drop(freed);
        bytes
    }

    /// Hold the pool empty and refuse returns while `constrained`; the next
    /// [`Self::maintain`] releases every idle entry.
    pub fn set_constrained(&self, constrained: bool) {
        let mut state = self.inner.lock();
        state.constrained = constrained;
        for class in state.classes.values_mut() {
            class.demand.set_constrained(constrained);
        }
    }

    /// Check out writable storage for `length` bytes.
    ///
    /// The capacity is the 64 KiB size class of `length` rounded up to the
    /// length unit, and `charge` must cover it. Recycled storage is not
    /// re-zeroed unless its previous charge required a wipe: producers must
    /// overwrite all `length` bytes. Rejects zero or oversized lengths and
    /// undercharging with `InvalidConfiguration`; allocation failure is `Busy`.
    pub fn take(&self, length: usize, charge: C) -> Result<DetachedBuffer<C>> {
        let unit = self.inner.alignment.length();
        let padded = length
            .checked_add(unit - 1)
            .map(|n| n / unit * unit)
            .filter(|n| length != 0 && *n <= Alignment::MAX_TRANSFER_LENGTH)
            .ok_or(Error::InvalidConfiguration)?;
        let capacity = buffer_capacity(padded).ok_or(Error::InvalidConfiguration)?;
        if !charge.covers(capacity) {
            return Err(Error::InvalidConfiguration);
        }
        let reused = {
            let mut state = self.inner.lock();
            let class = state.class(capacity);
            let idle = class.entries.pop();
            if idle.is_some() {
                let len = class.entries.len();
                class.demand.record_hit(len);
                state.bytes -= capacity;
                state.count -= 1;
            } else {
                class.demand.record_miss();
            }
            idle
        };
        let allocation = match reused {
            Some(Idle(mut allocation)) => {
                // The previous charge drops here, outside the lock.
                allocation.charge = charge;
                allocation
            }
            None => {
                let layout = Layout::from_size_align(capacity, self.inner.alignment.memory())
                    .map_err(|_| Error::InvalidConfiguration)?;
                let (pointer, release) = allocate_zeroed(layout)?;
                Allocation {
                    pointer,
                    layout,
                    release,
                    charge,
                    retained: Vec::new(),
                    clean: true,
                    #[cfg(test)]
                    wipes: WipeCount::default(),
                }
            }
        };
        Ok(DetachedBuffer {
            allocation: ManuallyDrop::new(allocation),
            length,
            unit,
            origin: Arc::downgrade(&self.inner),
        })
    }
}

/// Writable aligned storage from a [`WorkerBufferPool`] that may move to
/// another thread before being frozen.
///
/// Dropping an unfrozen buffer returns it to the originating pool.
#[must_use]
pub struct DetachedBuffer<C: Charge> {
    // Taken only by Drop or freeze.
    allocation: ManuallyDrop<Allocation<C>>,
    length: usize,
    unit: usize,
    origin: SyncWeak<SharedPool<C>>,
}

// SAFETY: the allocation never carries retained Rc guards, is exclusively
// owned, and the origin link is a sync::Weak to a Send + Sync pool.
unsafe impl<C: Charge + Send> Send for DetachedBuffer<C> {}

impl<C: Charge> Drop for DetachedBuffer<C> {
    fn drop(&mut self) {
        // SAFETY: Drop runs once; freeze forgets self after taking the allocation.
        let allocation = unsafe { ManuallyDrop::take(&mut self.allocation) };
        give_back(&self.origin, allocation);
    }
}

impl<C: Charge> fmt::Debug for DetachedBuffer<C> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("DetachedBuffer")
            .field("length", &self.length)
            .field("padded", &self.padded_len())
            .field("capacity", &self.capacity())
            .finish_non_exhaustive()
    }
}

impl<C: Charge> DetachedBuffer<C> {
    /// Requested length in bytes.
    #[must_use]
    pub fn len(&self) -> usize {
        self.length
    }

    /// Always false: construction rejects zero-length buffers.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        false
    }

    /// Length rounded up to the pool's direct-I/O length unit.
    #[must_use]
    pub fn padded_len(&self) -> usize {
        self.length.div_ceil(self.unit) * self.unit
    }

    /// Complete allocation capacity that must remain charged.
    #[must_use]
    pub fn capacity(&self) -> usize {
        self.allocation.layout.size()
    }

    /// Borrow the allocation's accounting owner.
    #[must_use]
    pub fn charge(&self) -> &C {
        &self.allocation.charge
    }

    /// Borrow the requested bytes, which may hold stale data until written.
    #[must_use]
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: initialized allocation of at least length bytes stays owned.
        unsafe { std::slice::from_raw_parts(self.allocation.pointer.as_ptr(), self.length) }
    }

    /// Exclusively borrow exactly the requested bytes.
    #[must_use]
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        let length = self.length;
        &mut self.allocation.as_mut_slice()[..length]
    }

    /// Shrink the requested length once a producer knows its final size.
    pub fn truncate(&mut self, length: usize) -> Result<()> {
        if length == 0 || length > self.length {
            return Err(Error::InvalidConfiguration);
        }
        self.length = length;
        Ok(())
    }

    /// Zero the pad up to [`Self::padded_len`] and freeze without copying.
    pub fn freeze(mut self) -> FrozenBuffer<C> {
        let (length, padded) = (self.length, self.padded_len());
        self.allocation.as_mut_slice()[length..padded].fill(0);
        // SAFETY: self is forgotten below, so Drop never takes the allocation twice.
        let allocation = unsafe { ManuallyDrop::take(&mut self.allocation) };
        let origin = std::mem::take(&mut self.origin);
        std::mem::forget(self);
        FrozenBuffer {
            allocation: ManuallyDrop::new(allocation),
            length,
            padded,
            origin,
        }
    }
}

// SAFETY: owned aligned backing is initialized, stable, and live until Drop.
unsafe impl<C: Charge> IoBuffer for AlignedBuffer<C> {
    type Error = Error;

    /// Expose initialized stable bytes to the runtime.
    fn bytes(&self) -> Result<&[u8]> {
        AlignedBuffer::bytes(self)
    }

    /// Give the runtime exclusive access without moving the allocation.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        AlignedBuffer::bytes_mut(self)
    }
}

/// Pure allocation tests, including unwind and reentrant accounting destructors.
#[cfg(test)]
mod buffer_tests {
    use super::*;
    use std::cell::Cell;

    #[test]
    fn freeze_detaches_pool_preserves_pointer_and_rejects_retained_guards() {
        fn send_sync<T: Send + Sync>() {}
        send_sync::<FrozenBuffer<()>>();
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let mut buffer = alignment.allocate(4096, ()).unwrap().pooled(&pool);
        buffer.as_mut_slice().fill(9);
        let pointer = buffer.as_slice().as_ptr() as usize;
        let frozen = buffer.freeze().unwrap();
        assert!(pool.borrow().entries.is_empty());
        drop(pool);
        std::thread::spawn(move || {
            assert_eq!(frozen.as_slice().as_ptr() as usize, pointer);
            assert_eq!(frozen.as_slice(), &[9; 4096]);
            assert_eq!(frozen.capacity(), 4096);
        })
        .join()
        .unwrap();
        let mut buffer = alignment.allocate(4096, ()).unwrap();
        buffer.retain(Rc::new(()));
        assert!(matches!(buffer.freeze(), Err(Error::InvalidConfiguration)));
    }

    #[test]
    fn frozen_mapping_keeps_full_capacity_and_rebind_rejects_undercharge() {
        struct Bytes(usize);
        impl Charge for Bytes {
            fn covers(&self, bytes: usize) -> bool {
                self.0 >= bytes
            }
            fn requires_wipe(&self) -> bool {
                false
            }
        }
        let length = 16 * 1024 * 1024 + 65536;
        let mut buffer = Alignment::new(4096, 4096, 4096)
            .unwrap()
            .allocate(length, Bytes(length))
            .unwrap();
        buffer.as_mut_slice()[length - 1] = 91;
        let pointer = buffer.as_slice().as_ptr() as usize;
        let mut frozen = buffer.freeze().unwrap();
        assert_eq!(
            frozen.rebind(Bytes(length - 1)),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(frozen.charge().0, length);
        frozen.rebind(Bytes(length)).unwrap();
        std::thread::spawn(move || {
            assert_eq!(frozen.capacity(), length);
            assert_eq!(frozen.as_slice().as_ptr() as usize, pointer);
            assert_eq!(frozen.as_slice()[length - 1], 91);
        })
        .join()
        .unwrap();
    }

    #[test]
    fn size_class_reuse_keeps_mismatches_and_full_capacity_charged() {
        let alignment = Alignment::new(4096, 1, 1).unwrap();
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let capacity = buffer_capacity(100_001).unwrap();
        let mut buffer = alignment.allocate(capacity, ()).unwrap().pooled(&pool);
        let pointer = buffer.as_slice().as_ptr();
        buffer.as_mut_slice().fill(0xa5);
        drop(buffer);
        assert!(pool.borrow_mut().take_for(capacity + 1, &()).is_none());
        let live = Rc::new(Cell::new(0));
        let charged = Rc::new(RefCell::new(BufferPool::default()));
        drop(
            alignment
                .allocate(capacity, TrackedCharge::new(&live, capacity))
                .unwrap()
                .pooled(&charged),
        );
        assert!(
            charged
                .borrow_mut()
                .take_for(100_002, &TrackedCharge::new(&live, 100_002))
                .is_none()
        );
        assert_eq!(live.get(), capacity);
        let buffer = pool.borrow_mut().take_for(100_002, &()).unwrap();
        assert_eq!(buffer.as_slice().as_ptr(), pointer);
        assert!(buffer.as_slice().iter().all(|&b| b == 0));
    }

    #[test]
    fn free_lists_bound_depth_and_bytes_under_size_flood() {
        let alignment = Alignment::new(64, 1, 1).unwrap();
        for budget in [128 * 1024, 128 * 1024 * 1024] {
            let live = Rc::new(Cell::new(0));
            let pool = Rc::new(RefCell::new(BufferPool {
                budget,
                demand: PoolDemand::new().with_target(8),
                ..BufferPool::default()
            }));
            for length in (1..=32).map(|n| n * 8192) {
                drop(
                    alignment
                        .allocate(length, TrackedCharge::new(&live, length))
                        .unwrap()
                        .pooled(&pool),
                );
                assert!(pool.borrow().entries.len() <= 8);
                assert!(live.get() <= budget);
            }
            if budget > 128 * 1024 {
                assert_eq!(pool.borrow().entries.len(), 8);
            }
            drop(pool);
            assert_eq!(live.get(), 0);
        }
    }

    #[cfg(all(target_os = "linux", not(miri)))]
    #[test]
    fn mmap_threshold_zero_alignment_and_matching_unmap() {
        for length in [HUGE_PAGE_BYTES - 1, HUGE_PAGE_BYTES, HUGE_PAGE_BYTES + 1] {
            let mut buffer = Alignment::new(4096, 1, 1)
                .unwrap()
                .allocate(length, ())
                .unwrap();
            assert!(buffer.as_slice().iter().all(|&b| b == 0));
            let pointer = buffer.as_slice().as_ptr();
            let mapped = matches!(buffer.allocation().release, Release::Mapping(_));
            assert_eq!(mapped, length >= HUGE_PAGE_BYTES);
            if mapped {
                assert!((pointer as usize).is_multiple_of(HUGE_PAGE_BYTES));
            }
            buffer.as_mut_slice()[length - 1] = 0xa5;
            drop(buffer);
            if mapped {
                let mut residency = 0u8;
                // SAFETY: mincore queries a numeric address without dereferencing
                // released storage; one output byte covers this one-page query.
                assert_eq!(
                    unsafe { libc::mincore(pointer.cast_mut().cast(), 1, &mut residency) },
                    -1
                );
                assert_eq!(
                    std::io::Error::last_os_error().raw_os_error(),
                    Some(libc::ENOMEM)
                );
            }
        }
        let buffer = Alignment::new(2 * HUGE_PAGE_BYTES, 1, 1)
            .unwrap()
            .allocate(HUGE_PAGE_BYTES, ())
            .unwrap();
        assert!((buffer.as_slice().as_ptr() as usize).is_multiple_of(2 * HUGE_PAGE_BYTES));
    }

    #[cfg(all(target_os = "linux", not(miri)))]
    #[test]
    fn public_mapping_discard_is_zero_on_reuse_without_secure_wipe() {
        struct Public;
        impl Charge for Public {
            fn covers(&self, _: usize) -> bool {
                true
            }
            fn requires_wipe(&self) -> bool {
                false
            }
        }
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let mut buffer = Alignment::new(4096, 1, 1)
            .unwrap()
            .allocate(HUGE_PAGE_BYTES, Public)
            .unwrap()
            .pooled(&pool);
        let pointer = buffer.as_slice().as_ptr();
        let wipes = buffer.allocation().wipes.clone();
        buffer.as_mut_slice().fill(0xa5);
        drop(buffer);
        let mut buffer = pool
            .borrow_mut()
            .take_for(HUGE_PAGE_BYTES, &Public)
            .unwrap();
        assert_eq!(buffer.as_slice().as_ptr(), pointer);
        assert!(buffer.as_slice().iter().all(|&b| b == 0));
        assert_eq!(wipes.get(), 0);
        buffer.as_mut_slice().fill(0xa5);
        drop(buffer);
        assert_eq!(wipes.get(), 0);
    }

    #[cfg(all(target_os = "linux", not(miri)))]
    #[test]
    fn impossible_mapping_returns_busy_without_exposing_storage() {
        let layout = Layout::from_size_align(isize::MAX as usize, 1).unwrap();
        assert!(matches!(allocate_zeroed(layout), Err(Error::Busy)));
    }

    /// Tracks primary and retained bytes without exposing a Debug implementation.
    struct TrackedCharge {
        live: Rc<Cell<usize>>,

        bytes: usize,
    }

    impl TrackedCharge {
        /// Admit and record a fixed number of live bytes.
        fn new(live: &Rc<Cell<usize>>, bytes: usize) -> Self {
            live.set(live.get() + bytes);
            Self {
                live: live.clone(),
                bytes,
            }
        }
    }

    impl Charge for TrackedCharge {
        /// Cover only the number of bytes admitted by this guard.
        fn covers(&self, bytes: usize) -> bool {
            self.bytes >= bytes
        }
    }

    impl Drop for TrackedCharge {
        /// Return admitted bytes exactly once.
        fn drop(&mut self) {
            self.live.set(self.live.get() - self.bytes);
        }
    }

    /// Full padded capacity is erased, and clean reuse does not erase twice.
    #[test]
    fn secure_wipe_covers_padding_and_skips_clean_pool_lifetimes() {
        let alignment = Alignment::new(4096, 512, 512).unwrap();
        let length = alignment.extent(0, 513).unwrap().length();
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let mut buffer = alignment.allocate(length, ()).unwrap().pooled(&pool);
        let wipes = buffer.allocation().wipes.clone();
        assert!(buffer.allocation().clean);
        assert_eq!(buffer.as_slice(), vec![0; 1024]);
        drop(buffer);
        assert_eq!(wipes.get(), 0);
        buffer = pool.borrow_mut().entries.pop_back().unwrap().pooled(&pool);
        buffer.as_mut_slice().fill(0xa5);
        assert!(!buffer.allocation().clean);
        drop(buffer);
        assert_eq!(wipes.get(), 1);
        buffer = pool.borrow_mut().entries.pop_back().unwrap().pooled(&pool);
        assert_eq!(buffer.as_slice(), vec![0; 1024]);
        assert!(buffer.allocation().clean);
        drop(buffer);
        drop(pool);
        assert_eq!(wipes.get(), 1);
    }

    /// Runtime pointer writes dirty storage before submission, including padding.
    #[test]
    fn kernel_write_borrow_dirties_clean_reused_storage() {
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let alignment = Alignment::new(64, 1, 1).unwrap();
        let mut buffer = alignment.allocate(128, ()).unwrap().pooled(&pool);
        let wipes = buffer.allocation().wipes.clone();
        for expected in 1..=2 {
            assert!(buffer.allocation().clean);
            let pointer = IoBuffer::bytes_mut(&mut buffer).unwrap().as_mut_ptr();
            // SAFETY: simulate a kernel completion while the exclusive owner is
            // retained, before any subsequent access or release of the buffer.
            unsafe { pointer.add(127).write(0x5a) };
            assert!(!buffer.allocation().clean);
            assert_eq!(buffer.as_slice()[127], 0x5a);
            drop(buffer);
            assert_eq!(wipes.get(), expected);
            buffer = pool.borrow_mut().entries.pop_back().unwrap().pooled(&pool);
            assert_eq!(IoBuffer::bytes(&buffer).unwrap(), &[0; 128]);
        }
        // Even an unused mutable borrow must conservatively require a wipe.
        let _ = buffer.bytes_mut().unwrap();
        drop(buffer);
        assert_eq!(wipes.get(), 3);
        drop(pool);
        assert_eq!(wipes.get(), 3);
    }

    /// Invalid transfers must fail before inspecting accounting or allocating.
    #[test]
    fn transfer_limit_is_enforced_before_allocation_or_charge_inspection() {
        /// Detects any accounting inspection on a structurally invalid request.
        struct UncheckedCharge;

        impl Charge for UncheckedCharge {
            /// Panic when validation reaches accounting in the wrong order.
            fn covers(&self, _: usize) -> bool {
                panic!("invalid lengths must be rejected before inspecting accounting");
            }
        }
        let alignment = Alignment::new(1, 1, 1).unwrap();
        let max = Alignment::MAX_TRANSFER_LENGTH;
        assert_eq!(Extent::new(0, max).unwrap().length(), max);
        assert_eq!(alignment.extent(0, max).unwrap().length(), max);
        for length in [0, max + 1, i32::MAX as usize, u32::MAX as usize, usize::MAX] {
            assert_eq!(Extent::new(0, length), Err(Error::Corrupt));
            assert_eq!(
                alignment.extent(0, length),
                Err(Error::InvalidConfiguration)
            );
            assert!(matches!(
                alignment.allocate(length, UncheckedCharge),
                Err(Error::InvalidConfiguration)
            ));
        }
        let alignment = Alignment::new(8, 3, 5).unwrap();
        let rounded_limit = max / 15 * 15;
        assert_eq!(
            alignment.extent(0, rounded_limit).unwrap().length(),
            rounded_limit
        );
        assert_eq!(
            alignment.extent(0, rounded_limit + 1),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(Extent::new(u64::MAX, 1), Err(Error::Corrupt));
        assert_eq!(alignment.extent(u64::MAX, 1), Err(Error::Corrupt));
        assert_eq!(Extent::new(u64::MAX - 1, 1).unwrap().offset(), u64::MAX - 1);
    }

    /// Non-power-of-two units use the LCM and reject overflow before allocation.
    #[test]
    fn arbitrary_units_preserve_lcm_rounding_and_detect_overflow() {
        for (offset_unit, length_unit, lcm) in [(3, 5, 15), (6, 9, 18), (768, 512, 1536)] {
            let alignment = Alignment::new(64, offset_unit, length_unit).unwrap();
            assert_eq!(alignment.memory(), 64);
            assert_eq!(alignment.offset(), offset_unit);
            assert_eq!(alignment.length(), length_unit);
            for (logical, expected) in [(1, lcm), (lcm, lcm), (lcm + 1, lcm * 2)] {
                let extent = alignment.extent(offset_unit, logical).unwrap();
                assert_eq!(extent.offset(), offset_unit);
                assert_eq!(extent.length(), expected);
                let buffer = alignment.allocate(expected, ()).unwrap();
                alignment.check(extent, &buffer).unwrap();
            }
            assert_eq!(alignment.extent(1, 1), Err(Error::InvalidConfiguration));
        }
        for alignment in [
            Alignment::new(1, u64::MAX, usize::MAX - 1).unwrap(),
            Alignment::new(1, 1, usize::MAX).unwrap(),
            Alignment::new(1, 2, usize::MAX).unwrap(),
        ] {
            assert_eq!(alignment.extent(0, 2), Err(Error::InvalidConfiguration));
        }
        for (memory, offset, length) in [(0, 1, 1), (3, 1, 1), (1, 0, 1), (1, 1, 0)] {
            assert_eq!(
                Alignment::new(memory, offset, length),
                Err(Error::Unsupported)
            );
        }
        assert_eq!(
            Alignment::new(1usize << (usize::BITS - 1), 1, 1),
            Err(Error::Unsupported)
        );
    }

    /// Moving a buffer preserves its address and initialized runtime-visible bytes.
    #[test]
    fn initialized_storage_remains_stable_across_moves_and_trait_access() {
        let alignment = Alignment::new(64, 3, 5).unwrap();
        let mut buffer = alignment.allocate(15, ()).unwrap();
        assert!(!buffer.is_empty());
        assert_eq!(buffer.len(), 15);
        assert_eq!(buffer.as_slice(), &[0; 15]);
        let pointer = IoBuffer::bytes_mut(&mut buffer).unwrap().as_mut_ptr();
        assert!((pointer as usize).is_multiple_of(64));
        let moved = std::hint::black_box(Some(buffer));
        // SAFETY: moving the owner preserves the allocation; no intervening reborrow.
        unsafe { pointer.write(42) };
        let mut buffer = moved.unwrap();
        assert_eq!(IoBuffer::bytes(&buffer).unwrap()[0], 42);
        buffer.bytes_mut().unwrap()[1] = 17;
        assert_eq!(buffer.bytes().unwrap()[1], 17);
        assert_eq!(buffer.as_slice().as_ptr(), pointer);
        alignment
            .check(Extent::new(3, 15).unwrap(), &buffer)
            .unwrap();
        for extent in [Extent::new(1, 15).unwrap(), Extent::new(3, 10).unwrap()] {
            assert_eq!(
                alignment.check(extent, &buffer),
                Err(Error::InvalidConfiguration)
            );
        }
        assert_eq!(
            Alignment::new(64, 3, 2)
                .unwrap()
                .check(Extent::new(3, 15).unwrap(), &buffer),
            Err(Error::InvalidConfiguration)
        );
    }

    /// Admission replacement is atomic and Debug does not expose guard internals.
    #[test]
    fn charge_validation_rebinding_and_debug_do_not_require_charge_debug() {
        let live = Rc::new(Cell::new(0));
        let alignment = Alignment::new(8, 3, 5).unwrap();
        for (length, charge) in [
            (15, 14),
            (14, 15),
            (0, 15),
            (Alignment::MAX_TRANSFER_LENGTH + 1, 15),
        ] {
            assert_eq!(
                alignment
                    .allocate(length, TrackedCharge::new(&live, charge))
                    .unwrap_err(),
                Error::InvalidConfiguration
            );
            assert_eq!(live.get(), 0);
        }
        let mut buffer = alignment
            .allocate(15, TrackedCharge::new(&live, 15))
            .unwrap();
        assert_eq!(
            buffer.rebind(TrackedCharge::new(&live, 14)),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(live.get(), 15);
        buffer.rebind(TrackedCharge::new(&live, 20)).unwrap();
        assert_eq!(live.get(), 20);
        let debug = format!("{buffer:?}");
        assert!(debug.contains("length: 15"));
        assert!(debug.contains("alignment: 8"));
        assert!(!debug.contains("TrackedCharge"));
        drop(buffer);
        assert_eq!(live.get(), 0);
    }

    /// Pooling retains primary accounting while releasing completion-only guards.
    #[test]
    fn pool_transfers_allocation_and_primary_charge_but_not_retained_guards() {
        let live = Rc::new(Cell::new(0));
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let alignment = Alignment::new(64, 3, 5).unwrap();
        let mut buffer = alignment
            .allocate(15, TrackedCharge::new(&live, 15))
            .unwrap()
            .pooled(&pool);
        let pointer = buffer.as_slice().as_ptr();
        buffer.as_mut_slice().fill(42);
        let extra = Rc::new(TrackedCharge::new(&live, 7));
        let weak = Rc::downgrade(&extra);
        buffer.retain(extra);
        assert_eq!(live.get(), 22);
        drop(buffer);
        assert_eq!(live.get(), 15);
        assert!(weak.upgrade().is_none());
        let mut reused = pool.borrow_mut().entries.pop_back().unwrap();
        assert_eq!(reused.as_slice().as_ptr(), pointer);
        assert_eq!(reused.as_slice(), &[0; 15]);
        reused.rebind(TrackedCharge::new(&live, 20)).unwrap();
        assert_eq!(live.get(), 20);
        drop(reused.pooled(&pool));
        drop(pool);
        assert_eq!(live.get(), 0);
    }

    /// An unusable return slot frees storage exactly once instead of panicking.
    #[test]
    fn unavailable_borrowed_and_occupied_pools_release_exactly_once() {
        let alignment = Alignment::new(8, 1, 1).unwrap();
        for scenario in 0..4 {
            let live = Rc::new(Cell::new(0));
            let pool = Rc::new(RefCell::new(BufferPool {
                demand: PoolDemand::new().with_bounds(0, 1).with_target(1),
                ..BufferPool::default()
            }));
            let mut buffer = alignment
                .allocate(8, TrackedCharge::new(&live, 8))
                .unwrap()
                .pooled(&pool);
            buffer.as_mut_slice().fill(0x5a);
            let wipes = buffer.allocation().wipes.clone();
            buffer.retain(Rc::new(TrackedCharge::new(&live, 3)));
            match scenario {
                0 => {
                    drop(pool);
                    drop(buffer);
                }
                1 => {
                    let borrow = pool.borrow();
                    drop(buffer);
                    assert_eq!(live.get(), 0);
                    assert!(borrow.entries.is_empty());
                }
                2 => {
                    let borrow = pool.borrow_mut();
                    drop(buffer);
                    assert_eq!(live.get(), 0);
                    assert!(borrow.entries.is_empty());
                }
                _ => {
                    let idle = alignment.allocate(8, TrackedCharge::new(&live, 8)).unwrap();
                    let pointer = idle.as_slice().as_ptr();
                    pool.borrow_mut().entries.push_back(idle);
                    drop(buffer);
                    assert_eq!(live.get(), 8);
                    assert_eq!(pool.borrow().entries[0].as_slice().as_ptr(), pointer);
                    drop(pool);
                }
            }
            assert_eq!(live.get(), 0);
            assert_eq!(wipes.get(), 1);
        }
    }

    /// Caller destructors run before the return slot is borrowed.
    #[test]
    fn retained_guard_can_inspect_pool_before_buffer_is_returned() {
        /// Runs a caller-provided destructor to test reentrant pool inspection.
        struct Guard(Option<Box<dyn FnOnce()>>);

        impl Charge for Guard {
            /// Admit all sizes for this destructor-order test.
            fn covers(&self, _: usize) -> bool {
                true
            }
        }

        impl Drop for Guard {
            /// Invoke the callback at most once.
            fn drop(&mut self) {
                if let Some(callback) = self.0.take() {
                    callback();
                }
            }
        }
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let called = Rc::new(Cell::new(false));
        let mut buffer = Alignment::new(8, 1, 1)
            .unwrap()
            .allocate(8, Guard(None))
            .unwrap()
            .pooled(&pool);
        let observed_pool = pool.clone();
        let observed_called = called.clone();
        buffer.as_mut_slice().fill(0x5a);
        let wipes = buffer.allocation().wipes.clone();
        let observed_wipes = wipes.clone();
        buffer.retain(Rc::new(Guard(Some(Box::new(move || {
            assert!(observed_pool.borrow_mut().entries.is_empty());
            assert_eq!(observed_wipes.get(), 1);
            observed_called.set(true);
        })))));
        drop(buffer);
        assert!(called.get());
        assert!(!pool.borrow().entries.is_empty());
        drop(pool);
        assert_eq!(wipes.get(), 1);
    }

    /// Unwinding through extra accounting cannot leak primary allocation ownership.
    #[test]
    fn retained_guard_unwind_still_drops_owned_allocation_and_primary_charge() {
        /// Counts destruction and optionally injects a panic.
        struct Guard {
            live: Rc<Cell<usize>>,

            panic: bool,
        }

        impl Charge for Guard {
            /// Admit all sizes for the unwind test.
            fn covers(&self, _: usize) -> bool {
                true
            }
        }

        impl Drop for Guard {
            /// Record release before injecting the requested destructor panic.
            fn drop(&mut self) {
                self.live.set(self.live.get() - 1);
                assert!(!self.panic, "injected retained guard panic");
            }
        }
        let live = Rc::new(Cell::new(2));
        let pool = Rc::new(RefCell::new(BufferPool::default()));
        let mut buffer = Alignment::new(8, 1, 1)
            .unwrap()
            .allocate(
                8,
                Guard {
                    live: live.clone(),
                    panic: false,
                },
            )
            .unwrap()
            .pooled(&pool);
        buffer.as_mut_slice().fill(0x5a);
        let wipes = buffer.allocation().wipes.clone();
        buffer.retain(Rc::new(Guard {
            live: live.clone(),
            panic: true,
        }));
        assert!(std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(buffer))).is_err());
        assert_eq!(live.get(), 0);
        assert!(pool.borrow().entries.is_empty());
        assert_eq!(wipes.get(), 1);
    }
}

/// Cross-thread worker pool and detached buffer tests.
#[cfg(test)]
mod shared_pool_tests {
    use super::*;
    use std::time::Instant;

    /// Charge whose wipe requirement and coverage are configurable.
    struct Secret {
        wipe: bool,
        limit: usize,
    }

    impl Charge for Secret {
        fn covers(&self, bytes: usize) -> bool {
            bytes <= self.limit
        }

        fn requires_wipe(&self) -> bool {
            self.wipe
        }
    }

    fn plain() -> Secret {
        Secret {
            wipe: false,
            limit: usize::MAX,
        }
    }

    fn secret() -> Secret {
        Secret {
            wipe: true,
            limit: usize::MAX,
        }
    }

    fn alignment() -> Alignment {
        Alignment::new(4096, 4096, 512).unwrap()
    }

    #[test]
    fn handles_are_thread_safe() {
        fn assert_send<T: Send>() {}
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send::<DetachedBuffer<()>>();
        assert_send_sync::<FrozenBuffer<()>>();
        assert_send_sync::<WorkerBufferPool<()>>();
    }

    #[test]
    fn take_aligns_pointer_and_rounds_padded_length() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(1000, plain()).unwrap();
        assert_eq!(buffer.len(), 1000);
        assert_eq!(buffer.padded_len(), 1024);
        assert_eq!(buffer.capacity(), 64 * 1024);
        assert!((buffer.as_slice().as_ptr() as usize).is_multiple_of(4096));
        assert_eq!(buffer.as_mut_slice().len(), 1000);
        let exact = pool.take(1024, plain()).unwrap();
        assert_eq!(exact.padded_len(), 1024);
    }

    #[test]
    fn take_rejects_invalid_lengths_and_undercharge() {
        let pool = WorkerBufferPool::new(alignment());
        assert!(matches!(
            pool.take(0, plain()),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            pool.take(Alignment::MAX_TRANSFER_LENGTH + 1, plain()),
            Err(Error::InvalidConfiguration)
        ));
        let under = Secret {
            wipe: false,
            limit: 4096,
        };
        assert!(matches!(
            pool.take(4096, under),
            Err(Error::InvalidConfiguration)
        ));
    }

    #[test]
    fn freeze_zeroes_pad_over_stale_recycled_data() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(1024, plain()).unwrap();
        buffer.as_mut_slice().fill(0xAA);
        let pointer = buffer.as_slice().as_ptr();
        drop(buffer);
        let mut buffer = pool.take(1000, plain()).unwrap();
        assert_eq!(buffer.as_slice().as_ptr(), pointer);
        // Non-wipe storage is not re-zeroed on reuse.
        assert_eq!(buffer.as_slice()[999], 0xAA);
        buffer.as_mut_slice().fill(7);
        let frozen = buffer.freeze();
        assert_eq!(frozen.as_slice(), &[7; 1000][..]);
        assert_eq!(frozen.padded_slice().len(), 1024);
        assert!(frozen.padded_slice()[1000..].iter().all(|&b| b == 0));
    }

    #[test]
    fn truncate_shrinks_length_before_freeze() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(4096, plain()).unwrap();
        buffer.as_mut_slice().fill(3);
        assert!(buffer.truncate(0).is_err());
        assert!(buffer.truncate(4097).is_err());
        buffer.truncate(600).unwrap();
        let frozen = buffer.freeze();
        assert_eq!(frozen.as_slice().len(), 600);
        assert_eq!(frozen.padded_slice().len(), 1024);
        assert!(frozen.padded_slice()[600..].iter().all(|&b| b == 0));
    }

    #[test]
    fn wipe_class_is_wiped_on_return_and_reused() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(512, secret()).unwrap();
        let wipes = buffer.allocation.wipes.clone();
        buffer.as_mut_slice().fill(5);
        let frozen = buffer.freeze();
        assert_eq!(wipes.get(), 0);
        drop(frozen);
        assert_eq!(wipes.get(), 1);
        assert_eq!(pool.stats().idle_buffers, 1);
        let buffer = pool.take(512, plain()).unwrap();
        assert!(buffer.as_slice().iter().all(|&b| b == 0));
    }

    #[test]
    fn non_wipe_class_is_reused_without_wipe() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(512, plain()).unwrap();
        let wipes = buffer.allocation.wipes.clone();
        buffer.as_mut_slice().fill(1);
        let pointer = buffer.as_slice().as_ptr();
        drop(buffer.freeze());
        assert_eq!(wipes.get(), 0);
        assert_eq!(
            pool.stats(),
            SharedPoolStats {
                idle_buffers: 1,
                idle_bytes: 64 * 1024
            }
        );
        assert_eq!(
            pool.take(512, plain()).unwrap().as_slice().as_ptr(),
            pointer
        );
    }

    #[test]
    fn class_depth_and_budget_limit_idle_storage() {
        let pool = WorkerBufferPool::new(alignment());
        let buffers: Vec<_> = (0..SHARED_POOL_CLASS_DEPTH + 2)
            .map(|_| pool.take(512, plain()).unwrap())
            .collect();
        drop(buffers);
        assert_eq!(pool.stats().idle_buffers, SHARED_POOL_CLASS_DEPTH);
        // A different class has its own depth.
        drop(pool.take(64 * 1024 + 1, plain()).unwrap());
        assert_eq!(pool.stats().idle_buffers, SHARED_POOL_CLASS_DEPTH + 1);
        pool.clear();
        assert_eq!(pool.stats(), SharedPoolStats::default());

        let small = WorkerBufferPool::with_budget(alignment(), 128 * 1024);
        let buffers: Vec<_> = (0..3).map(|_| small.take(512, plain()).unwrap()).collect();
        drop(buffers);
        assert_eq!(small.stats().idle_bytes, 128 * 1024);
        assert_eq!(small.stats().idle_buffers, 2);
    }

    #[test]
    fn drop_on_other_thread_returns_to_origin() {
        let pool = WorkerBufferPool::new(alignment());
        let mut buffer = pool.take(512, secret()).unwrap();
        let wipes = buffer.allocation.wipes.clone();
        let pointer = buffer.as_slice().as_ptr() as usize;
        buffer.as_mut_slice().fill(9);
        let frozen = std::thread::spawn(move || buffer.freeze()).join().unwrap();
        let shared = Arc::new(frozen);
        let other = Arc::clone(&shared);
        std::thread::spawn(move || assert_eq!(other.as_slice()[0], 9))
            .join()
            .unwrap();
        std::thread::spawn(move || drop(shared)).join().unwrap();
        assert_eq!(wipes.get(), 1);
        assert_eq!(pool.stats().idle_buffers, 1);
        assert_eq!(
            pool.take(512, plain()).unwrap().as_slice().as_ptr() as usize,
            pointer
        );
        // Unfrozen buffers also return from another thread.
        let detached = pool.take(512, plain()).unwrap();
        std::thread::spawn(move || drop(detached)).join().unwrap();
        assert_eq!(pool.stats().idle_buffers, 1);
    }

    #[test]
    fn drop_after_pool_is_gone_frees() {
        let pool = WorkerBufferPool::new(alignment());
        let detached = pool.take(512, secret()).unwrap();
        let wipes = detached.allocation.wipes.clone();
        let mut other = pool.take(512, plain()).unwrap();
        other.as_mut_slice().fill(1);
        let frozen = other.freeze();
        drop(pool);
        std::thread::spawn(move || drop(frozen)).join().unwrap();
        drop(detached);
        // Clean storage needs no wipe.
        assert_eq!(wipes.get(), 0);
    }

    #[test]
    fn large_buffer_round_trips_through_mapping() {
        let pool = WorkerBufferPool::new(alignment());
        let length = 16 * 1024 * 1024 + 16;
        let mut buffer = pool.take(length, plain()).unwrap();
        assert!(buffer.capacity() >= HUGE_PAGE_BYTES);
        #[cfg(all(target_os = "linux", not(miri)))]
        assert!(matches!(buffer.allocation.release, Release::Mapping(_)));
        buffer.as_mut_slice().fill(4);
        let pointer = buffer.as_slice().as_ptr();
        let frozen = buffer.freeze();
        assert_eq!(frozen.padded_slice().len(), length.div_ceil(512) * 512);
        assert!(frozen.padded_slice()[length..].iter().all(|&b| b == 0));
        drop(frozen);
        assert_eq!(
            pool.take(length, plain()).unwrap().as_slice().as_ptr(),
            pointer
        );
    }

    #[test]
    fn aligned_buffer_freeze_is_unchanged_and_padded_equals_slice() {
        let pool = WorkerBufferPool::<()>::new(alignment());
        let buffer = alignment().allocate(4096, ()).unwrap();
        let frozen = buffer.freeze().unwrap();
        assert_eq!(frozen.padded_slice().len(), frozen.as_slice().len());
        drop(frozen);
        assert_eq!(pool.stats(), SharedPoolStats::default());
    }

    const KIB64: usize = 64 * 1024;

    /// Fill the pool with one idle buffer per listed length, returned in order.
    fn park(pool: &WorkerBufferPool<Secret>, lengths: &[usize]) -> Vec<*const u8> {
        let buffers: Vec<_> = lengths
            .iter()
            .map(|&length| pool.take(length, plain()).unwrap())
            .collect();
        let pointers = buffers.iter().map(|b| b.as_slice().as_ptr()).collect();
        for buffer in buffers {
            drop(buffer);
        }
        pointers
    }

    #[test]
    fn fresh_pool_retains_full_class_depth() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096; SHARED_POOL_CLASS_DEPTH + 1]);
        let stats = pool.pool_stats();
        assert_eq!(stats.entries, SHARED_POOL_CLASS_DEPTH);
        assert_eq!(stats.target, SHARED_POOL_CLASS_DEPTH);
        assert_eq!(stats.max_depth, SHARED_POOL_CLASS_DEPTH);
        assert_eq!(stats.misses, SHARED_POOL_CLASS_DEPTH as u64 + 1);
        assert_eq!(stats.rejects, 1);
    }

    #[test]
    fn maintain_releases_idle_entries_over_windows() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096; 4]);
        let start = Instant::now();
        assert_eq!(pool.maintain(start), 0, "first tick starts the window");
        assert_eq!(pool.maintain(start + DEFAULT_WINDOW / 2), 0);
        let mut now = start;
        let mut windows = 0;
        while pool.stats().idle_buffers > 0 {
            now += DEFAULT_WINDOW;
            windows += 1;
            assert!(pool.maintain(now) > 0, "every idle window releases");
            assert!(windows < 10);
        }
        assert_eq!(pool.stats(), SharedPoolStats::default());
        assert_eq!(pool.pool_stats().trimmed, 4);
    }

    #[test]
    fn maintain_releases_oldest_returns_first_within_each_class() {
        let pool = WorkerBufferPool::new(alignment());
        // Returned in order: 64 KiB, 128 KiB, 64 KiB, 128 KiB.
        let pointers = park(&pool, &[4096, 2 * KIB64, 4096, 2 * KIB64]);
        let start = Instant::now();
        pool.maintain(start);
        assert_eq!(pool.maintain(start + DEFAULT_WINDOW), 2 * KIB64 + KIB64);
        // The two newest remain: index 2 (64 KiB) and index 3 (128 KiB).
        assert_eq!(pool.pool_stats().entries, 2);
        assert_eq!(
            pool.take(4096, plain()).unwrap().as_slice().as_ptr(),
            pointers[2]
        );
        assert_eq!(
            pool.take(2 * KIB64, plain()).unwrap().as_slice().as_ptr(),
            pointers[3]
        );
    }

    #[test]
    fn busy_reuse_keeps_entries() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096; 4]);
        let start = Instant::now();
        pool.maintain(start);
        for window in 1..=30 {
            // Each window drains the pool to zero and refills it.
            park(&pool, &[4096; 4]);
            assert_eq!(pool.maintain(start + DEFAULT_WINDOW * window), 0);
            assert_eq!(pool.stats().idle_buffers, 4);
        }
        let stats = pool.pool_stats();
        assert_eq!(stats.misses, 4);
        assert_eq!(stats.trimmed, 0);
    }

    #[test]
    fn sustained_mixed_class_reuse_keeps_full_depth_in_each_class() {
        let pool = WorkerBufferPool::new(alignment());
        let lengths: Vec<usize> = [4096; SHARED_POOL_CLASS_DEPTH]
            .into_iter()
            .chain([2 * KIB64; SHARED_POOL_CLASS_DEPTH])
            .collect();
        park(&pool, &lengths);
        let start = Instant::now();
        pool.maintain(start);
        for window in 1..=30 {
            park(&pool, &lengths);
            assert_eq!(pool.maintain(start + DEFAULT_WINDOW * window), 0);
            assert_eq!(pool.stats().idle_buffers, 2 * SHARED_POOL_CLASS_DEPTH);
        }
        let stats = pool.pool_stats();
        assert_eq!(stats.misses, 2 * SHARED_POOL_CLASS_DEPTH as u64);
        assert_eq!((stats.trimmed, stats.rejects), (0, 0));
    }

    #[test]
    fn separate_single_class_reuse_is_not_trimmed() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096, 2 * KIB64]);
        let start = Instant::now();
        pool.maintain(start);
        for window in 1..=30 {
            // Each class drains to zero on its own while the other stays parked.
            park(&pool, &[4096]);
            park(&pool, &[2 * KIB64]);
            assert_eq!(pool.maintain(start + DEFAULT_WINDOW * window), 0);
            assert_eq!(pool.stats().idle_buffers, 2);
        }
        let stats = pool.pool_stats();
        assert_eq!((stats.misses, stats.trimmed), (2, 0));
    }

    #[test]
    fn idle_class_decays_and_is_dropped_while_busy_class_stays() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096, 2 * KIB64]);
        let start = Instant::now();
        pool.maintain(start);
        let mut window = 0;
        while pool.inner.lock().classes.contains_key(&(2 * KIB64)) {
            window += 1;
            assert!(window < 200, "idle class never decayed");
            park(&pool, &[4096]);
            pool.maintain(start + DEFAULT_WINDOW * window);
        }
        assert!(pool.inner.lock().classes.contains_key(&KIB64));
        let stats = pool.pool_stats();
        assert_eq!(stats.trimmed, 1, "only the idle 128 KiB buffer is trimmed");
    }

    #[test]
    fn constrained_refuses_returns_and_maintain_releases_all() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096, 2 * KIB64]);
        pool.set_constrained(true);
        assert!(pool.pool_stats().constrained);
        assert_eq!(pool.maintain(Instant::now()), 3 * KIB64);
        park(&pool, &[4096]);
        assert_eq!(pool.stats(), SharedPoolStats::default());
        pool.set_constrained(false);
        park(&pool, &[4096]);
        assert_eq!(pool.stats().idle_buffers, 1);
    }

    #[test]
    fn clear_records_trim() {
        let pool = WorkerBufferPool::new(alignment());
        park(&pool, &[4096; 3]);
        pool.clear();
        let stats = pool.pool_stats();
        assert_eq!((stats.entries, stats.bytes, stats.trimmed), (0, 0, 3));
    }

    /// Charge whose destructor proves the pool lock is not held.
    struct Probe {
        pool: Arc<std::sync::OnceLock<SyncWeak<SharedPool<Probe>>>>,
        drops: Arc<std::sync::atomic::AtomicUsize>,
    }

    impl Charge for Probe {
        fn covers(&self, _bytes: usize) -> bool {
            true
        }
    }

    impl Drop for Probe {
        fn drop(&mut self) {
            if let Some(pool) = self.pool.get().and_then(SyncWeak::upgrade) {
                assert!(pool.state.try_lock().is_ok(), "freed under the pool lock");
            }
            self.drops
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        }
    }

    #[test]
    fn maintain_frees_outside_the_lock() {
        let pool = WorkerBufferPool::new(alignment());
        let link = Arc::new(std::sync::OnceLock::new());
        link.set(Arc::downgrade(&pool.inner)).unwrap();
        let drops = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let probe = || Probe {
            pool: Arc::clone(&link),
            drops: Arc::clone(&drops),
        };
        let buffers = [
            pool.take(4096, probe()).unwrap(),
            pool.take(4096, probe()).unwrap(),
        ];
        drop(buffers);
        pool.set_constrained(true);
        assert_eq!(pool.maintain(Instant::now()), 2 * KIB64);
        assert_eq!(drops.load(std::sync::atomic::Ordering::Relaxed), 2);
    }
}
