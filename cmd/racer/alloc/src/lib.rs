//! Worker-local aligned storage with caller-owned accounting and I/O policy.
//!
//! This internal crate owns generic bytes and caller-keyed retention indexes, not
//! key schemas, records, encryption, integrity checks, versions, admission classes,
//! or a persistence protocol. The caller
//! supplies accounting guards, a reactor, cancellation policy, and index callbacks.
//! There is no assumed OS page size and no cross-thread allocation authority.
//!
//! # Startup and binding
//!
//! Construct [`Slab::new`] with a full file path and [`Segments::new`] with the
//! same segment size, then call [`Slab::open_configured`] outside the worker's
//! latency-sensitive path. Directory traversal, locking, sizing, and probing are
//! blocking. Alignment comes from `statx(STATX_DIOALIGN)` rather than a fixed page
//! size. A configured partial table must match capacity, segment size, and
//! alignment. The slab reports physical geometry even with a partial table.
//!
//! Opening and binding are separate steps: failed binding can leave the file open
//! and locked but cannot authorize I/O. Fix the table and retry or drop the slab.
//! Rebinding to a different table is rejected even when its geometry matches.
//! The maximum record size only checks that its padded size fits a segment at
//! startup; it is not an allocation quota or a per-submission record-size limit.
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
//! [`Slab::from_devices`] instead accepts one [`DevicePlacement`] per logical
//! segment. The caller opens devices with O_EXCL and O_DIRECT and supplies common
//! alignment. Bounds use BLKGETSIZE64 for block devices and length for regular
//! files. Placements can share an `Arc<File>` to use one
//! runtime descriptor per device. Startup never creates, resizes, or locks these
//! files. Logical extents still use segment-table offsets; submissions translate
//! them to the placement's physical range after checking lease bounds. Overlap
//! checks only compare the same inode or device identity within one slab. They
//! cannot detect whole-disk, partition, or device-mapper aliases. The caller must
//! guarantee disjoint physical storage across aliases and slabs, keep exclusive
//! ownership, and not change file flags or sizes while in use.
//!
//! In file mode, empty files are sparsely extended to capacity; nonempty size mismatches are
//! rejected without truncation. Capacity is a logical bound, not reserved disk
//! space, so later writes can fail with ENOSPC. Recycling changes metadata only:
//! it does not erase, truncate, or hole-punch disk bytes. Buffer zeroization is
//! not secure erasure of the file, and physical blocks can survive eviction.
//!
//! # Alignment and accounting
//!
//! [`Alignment`] permits arbitrary positive offset and length units; memory
//! alignment must be a supported power of two. Extent padding uses their least
//! common multiple, so offset unit 768 and length unit 512 require multiples of
//! 1536. Arithmetic and the conservative 1 GiB transfer cap are checked before
//! allocating. Larger logical records must be split by the caller. An [`Extent`]
//! alone checks a range, not its alignment or permission to access a segment.
//!
//! [`buf::Buf`] owns initialized stable storage that cannot be resized. Its
//! [`Charge`] must truthfully account for its entire padded size; `()` opts out
//! of accounting. [`Alignment::allocate`] builds unpooled storage, while
//! [`buf::Pool`] reuses it: idle memory is charged to the pool's [`buf::PoolAccount`],
//! and a checkout must bring a charge covering the full capacity. Outstanding
//! buffers do not keep the pool alive. Reclaim reaches idle bytes only, never
//! memory still owned by the kernel.
//!
//! # Completion ownership
//!
//! For writes, round the logical size, append the padded length, allocate matching
//! storage, copy bytes into the zeroed buffer, and submit [`Slab::write`]. Publish
//! the caller's mapping only after the write succeeds. Append reserves space and
//! does not roll it back on failed writes. For reads, validate the stored slot,
//! generation, and extent, acquire a lease, allocate storage, and submit a read.
//! A lease authorizes the segment's used prefix at acquisition, not just the last
//! appended record; it cannot authorize bytes appended afterward.
//! Any lease, including one from [`Segments::lease`], permits writes in that prefix.
//! Leases do not grant exclusive record ownership. The trusted caller must write
//! only reserved extents it owns and never overwrite published or readable records.
//! A lease does not prove initialization in its generation: append reserves space
//! without writing it. Recycled bytes may come from an earlier generation or a
//! different cache. Before exposing them as a valid record, the caller must check
//! record integrity, authentication, and cache identity, even after a successful read.
//!
//! Submission checks table identity, alignment, length, segment boundaries, and
//! captured used bytes. Reads and writes reject short completions. Accepted I/O
//! owns the buffer, all charges, and the lease through the runtime completion
//! fence, even when its waiting future is dropped. Writes also own their counter
//! guard. An unpolled future has submitted nothing. Continue driving the reactor
//! to complete or cancel accepted work before expecting resources to be released.
//! [`Slab::fence_writes`] neither waits for reads nor prevents new writes. Stop
//! admitting writes first if quiescence is required. It is a completion fence,
//! not a durability barrier: it never calls fsync or fdatasync.
//!
//! # Recovery and reclamation
//!
//! Slots normally progress from Free to Open to Sealed to Evicting to Free.
//! Append selects a fitting open tail or the lowest free slot. Full segments and
//! rotated tails become sealed. A valid rollover with no free slot seals the old
//! tail before returning Busy, enabling reclamation and retry. Malformed requests
//! and lease-counter overflow leave the table unchanged. New leases reject
//! eviction, but existing leases remain usable. Recycling requires no remaining
//! leases and increments generation without wrapping.
//!
//! A [`FreezeGuard`] blocks table mutation, not reads, snapshots, new read leases,
//! caller index mutation, or accepted I/O. It may outlive its table. Only one guard
//! exists at a time. Restore requires both thawing and draining all leases,
//! including reads and abandoned I/O. It validates the whole ordered image before
//! publication, seals an open tail, rebuilds free slots, and advances an epoch
//! that clears the eviction cursor. Invalid or busy restores
//! never partially publish.
//! Raw file opening, table configuration, and manual eviction are test-support
//! escape hatches. Production startup binds with [`Slab::open_configured`], and
//! [`disk_index::DiskIndex::reclaim_scored`] owns the ordering of mapping removal
//! before physical reuse.
//! The table and slab are cache-line aligned to isolate worker-local state;
//! their Rc ownership prevents moving authority to a different worker thread.
//!
//! For a checkpoint, stop mutation admission, retain a freeze guard, drive writes
//! through their completion fence, then capture compatible allocator and caller
//! metadata. Keep admission coordinated around thaw and restore. Neither the
//! image nor the completion fence proves durability or record validity. Recovery
//! must validate identity, lengths, integrity, and versions. Invalid, stale, torn,
//! or missing cache records must become misses and be refetched.
//!
//! The disk index keeps one clock hand across bounded sweeps.
//! Physical reclamation selects only Sealed/Evicting slots and recycles empty slots
//! after leases drain. Scored reclamation visits at most
//! min(slot count, max_visits, 64) slots, including skipped slots, then ranks
//! eligible candidates. For a nonzero reserve,
//! [`disk_index::DiskIndex::reclaim_scored`] can succeed once the reserve (capped at the
//! slot count) is met, even with pending evictions. Success is not a drain guarantee.
//! Busy means the bounded sweep could not meet the reserve, or table mutation was
//! blocked. Use [`Segments::free_count`] to check capacity and retry bounded
//! reclamation when more free slots are needed rather than spinning.
//! A zero reserve is a no-op, not a drain request; a zero entry budget
//! can recycle empty slots but cannot begin populated eviction. Scored
//! reclamation is the only eviction: there is no per-page victim selection and
//! no second-chance read mark. To free index entries rather than space, ask for
//! one more free slot than is free or evicting; [`Segments::seal_open`] makes
//! the open tail eligible when it holds every reclaimable record. There is no
//! compaction. [`SegmentEntries`] implementations
//! must compare current mappings before removal and report within budget; an
//! error cannot undo callback side effects. Freeze does not block index sweeps.
//!
//! # Validation
//!
//! Run `cargo test -p page-alloc` under an external TERM timeout. The crate has
//! no features, so there is nothing to toggle.
//! Simulation provides deterministic fault and cancellation ordering, not proof
//! of filesystem security or kernel support. Descriptor-replacement test hooks
//! require an open slab and no live writes and do not revalidate geometry.
//! Real tests print explicit capability skips with `-- --nocapture`; set
//! `PAGE_ALLOC_REQUIRE_REAL_IO=1` to turn those skips into failures. A skip is not
//! evidence that the real path passed. Miri can cover the pure `buf` unit tests,
//! `geometry_tests`, and `segments::tests` filters, but not real kernel I/O.
#![deny(unsafe_op_in_unsafe_fn)]
#![deny(missing_docs)]

pub mod buf;
mod demand;
pub mod disk_index;
pub mod intern;
#[cfg(all(target_os = "linux", not(miri)))]
mod numa;
pub mod pending;
mod segments;
mod sync;

mod slab;
pub mod stream;

pub use demand::{
    MAX_HOLD, MIN_HOLD, Plan, PoolStats, Pressure, Provision, ProvisionConfig, SAMPLE_PERIOD,
};
pub use segments::{
    FreezeGuard, Generation, SegmentId, SegmentLease, SegmentSnapshot, SegmentState, Segments,
};
pub use slab::{DevicePlacement, Slab};
use std::{
    alloc::{Layout, alloc_zeroed},
    fmt,
    ptr::NonNull,
};

/// Storage failures, separated from application record and admission policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
pub enum Error {
    /// The filesystem or kernel cannot provide required direct-I/O support.
    Unsupported,

    /// Caller configuration or an implementation contract is invalid.
    InvalidConfiguration,

    /// A transient lease, freeze, or resource limit prevents progress.
    Busy,

    /// An extent or persisted allocator image is malformed.
    Corrupt,

    /// A generation, segment state, or table identity is no longer valid.
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

/// Maximum number of slots retained by a segment allocation table.
pub const MAX_SEGMENTS: u64 = 1_000_000;

/// Validated physical dimensions, independent of record formats and slot quotas.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SegmentGeometry {
    slab_bytes: u64,

    segment_bytes: u64,

    segment_count: u64,

    alignment: Alignment,
}

impl SegmentGeometry {
    /// Validate physical geometry; retained tables separately enforce MAX_SEGMENTS.
    pub fn new(
        slab_bytes: u64,
        segment_bytes: u64,
        segment_count: u64,
        alignment: Alignment,
    ) -> Result<Self> {
        if segment_bytes == 0
            || slab_bytes == 0
            || segment_count == 0
            || !slab_bytes.is_multiple_of(segment_bytes)
            || segment_count > slab_bytes / segment_bytes
            || !segment_bytes.is_multiple_of(alignment.offset())
            || !segment_bytes.is_multiple_of(alignment.length() as u64)
            || segment_count.checked_mul(segment_bytes).is_none()
        {
            return Err(Error::Corrupt);
        }
        Ok(Self {
            slab_bytes,
            segment_bytes,
            segment_count,
            alignment,
        })
    }

    /// Full logical size of the physical file.
    pub fn slab_bytes(self) -> u64 {
        self.slab_bytes
    }

    /// Fixed physical size of each segment.
    pub fn segment_bytes(self) -> u64 {
        self.segment_bytes
    }

    /// Number of exposed segments, possibly less than the physical file allows.
    pub fn segment_count(self) -> u64 {
        self.segment_count
    }

    /// Direct-I/O requirements validated with these dimensions.
    pub fn alignment(self) -> Alignment {
        self.alignment
    }

    /// Compare table dimensions only, not occupancy or alignment compatibility.
    #[cfg(test)]
    fn matches_segments(&self, segments: &Segments) -> bool {
        segments.capacity_bytes() == self.slab_bytes
            && segments.segment_bytes() == self.segment_bytes
            && segments.count() as u64 == self.segment_count
    }
}

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

    /// Allocate zeroed, unpooled stable storage and retain its primary
    /// accounting guard. Length must be nonzero, length-aligned, covered by
    /// `charge`, and no larger than [`Self::MAX_TRANSFER_LENGTH`]. Allocation
    /// failure returns `Busy`. Use a [`buf::Pool`] to reuse storage instead.
    pub fn allocate<C: Charge>(&self, length: usize, charge: C) -> Result<buf::Buf<C>> {
        if length == 0 || length > Self::MAX_TRANSFER_LENGTH || !length.is_multiple_of(self.length)
        {
            return Err(Error::InvalidConfiguration);
        }
        let wipe = charge.requires_wipe();
        buf::Buf::unpooled_exact(length, self.memory, charge, wipe)
    }

    /// Validate the address, file offset, transfer unit, and exact buffer length.
    pub fn check<C: Charge>(&self, extent: Extent, buffer: &buf::Buf<C>) -> Result<()> {
        if !(buffer.as_ptr() as usize).is_multiple_of(self.memory)
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

/// Size class granule used to reserve staging for large records.
const STAGING_GRANULE: usize = 64 * 1024;

/// Round backing capacity without changing the length of an I/O transfer.
pub fn buffer_capacity(length: usize) -> Option<usize> {
    length
        .checked_add(STAGING_GRANULE - 1)
        .map(|n| n / STAGING_GRANULE * STAGING_GRANULE)
}

/// Pure geometry boundary tests independent of file I/O and checkpoint formats.
#[cfg(test)]
mod geometry_tests {
    use super::*;

    /// Standard test units, not a host-page-size assumption.
    fn alignment() -> Alignment {
        Alignment::new(512, 512, 512).unwrap()
    }

    /// Physical geometry does not impose the retained table's slot bound.
    #[test]
    fn geometry_accepts_partial_tables_without_checkpoint_item_policy() {
        let geometry = SegmentGeometry::new(4096, 1024, 2, alignment()).unwrap();
        assert_eq!(geometry.slab_bytes(), 4096);
        assert_eq!(geometry.segment_bytes(), 1024);
        assert_eq!(geometry.segment_count(), 2);
        assert_eq!(geometry.alignment(), alignment());
        assert!(SegmentGeometry::new(1024 * MAX_SEGMENTS, 1024, MAX_SEGMENTS, alignment()).is_ok());
        assert!(
            SegmentGeometry::new(
                1024 * (MAX_SEGMENTS + 1),
                1024,
                MAX_SEGMENTS + 1,
                alignment()
            )
            .is_ok()
        );
        assert!(
            SegmentGeometry::new(u64::MAX, 1, u64::MAX, Alignment::new(1, 1, 1).unwrap()).is_ok()
        );
    }

    /// Reject zero units, inconsistent capacity, misalignment, and overflow.
    #[test]
    fn geometry_rejects_zero_misalignment_capacity_and_overflow() {
        for (slab, segment, count) in [
            (0, 512, 1),
            (512, 0, 1),
            (512, 512, 0),
            (513, 512, 1),
            (512, 512, 2),
            (514, 257, 2),
            (u64::MAX, u64::MAX, 2),
        ] {
            assert_eq!(
                SegmentGeometry::new(slab, segment, count, alignment()),
                Err(Error::Corrupt)
            );
        }
        assert_eq!(
            SegmentGeometry::new(1024, 1024, 1, Alignment::new(512, 512, 768).unwrap()),
            Err(Error::Corrupt)
        );
    }

    /// Table matching ignores occupancy but compares each configured dimension.
    #[test]
    fn matching_live_table_ignores_occupancy_but_requires_dimensions() {
        let geometry = SegmentGeometry::new(4096, 1024, 2, alignment()).unwrap();
        let segments = Segments::new(1024);
        assert!(!geometry.matches_segments(&segments));
        segments.configure(4096, 2, alignment()).unwrap();
        assert!(geometry.matches_segments(&segments));
        let _held = segments.append(512).unwrap();
        assert!(geometry.matches_segments(&segments));
        assert!(
            !SegmentGeometry::new(4096, 1024, 3, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
        assert!(
            !SegmentGeometry::new(2048, 1024, 2, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
        assert!(
            !SegmentGeometry::new(4096, 512, 2, alignment())
                .unwrap()
                .matches_segments(&segments)
        );
    }

    /// Segment boundaries must satisfy both direct-I/O units simultaneously.
    #[test]
    fn divisibility_by_each_unit_is_equivalent_to_lcm_divisibility() {
        let alignment = Alignment::new(512, 512, 768).unwrap();
        assert!(SegmentGeometry::new(3072, 1536, 2, alignment).is_ok());
        assert_eq!(
            SegmentGeometry::new(2048, 1024, 2, alignment),
            Err(Error::Corrupt)
        );
        assert_eq!(
            SegmentGeometry::new(1536, 768, 2, alignment),
            Err(Error::Corrupt)
        );
        assert!(SegmentGeometry::new(u64::MAX, 1, 1, Alignment::new(1, 1, 1).unwrap()).is_ok());
    }
}

/// Buffer geometry and lifetime behavior independent of a pool.
#[cfg(test)]
mod buffer_tests {
    use super::*;
    use racer_io::ring::IoBuffer;

    #[cfg(all(target_os = "linux", not(miri)))]
    #[test]
    fn impossible_mapping_returns_busy_without_exposing_storage() {
        let layout = Layout::from_size_align(isize::MAX as usize, 1).unwrap();
        assert!(matches!(allocate_zeroed(layout), Err(Error::Busy)));
    }

    /// Invalid transfers fail before inspecting accounting or allocating.
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
        buffer.as_mut_slice()[1] = 17;
        assert_eq!(buffer.as_slice()[1], 17);
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
}
