//! Process-wide plaintext slab that clients map read-only.
//!
//! The slab is one sealed memfd, mapped once, split into fixed slots of
//! [`SLOT_BYTES`]. Delivery passes a read-only descriptor to a client once per
//! response, then names each page by its offset inside the slab, so page bytes
//! are never copied into socket buffers.
//!
//! Plaintext must stay an ordinary `Vec<u8>` for the rest of the dataplane, so
//! slab slots are handed out through [`SharedAllocator`], a global allocator
//! that wraps [`System`]. Only allocations made inside [`zeroed_vec`] (or
//! [`vec_with_capacity`]) land in the slab. Every other allocation goes to
//! [`System`] untouched. Deallocation recognizes slab pointers by address, so a
//! slab-backed `Vec` can be dropped, wiped, pooled, or zeroized like any other.
//!
//! On hosts with several NUMA nodes the slots are split into one range per
//! node, each bound to its node. A thread takes slots from its own node's range
//! first, so decrypt writes and client reads stay local.
//!
//! A binary opts in by declaring `SharedAllocator` as its global allocator and
//! calling [`install`]. Without both, [`zeroed_vec`] returns `None` and callers
//! fall back to private allocations.

use std::{
    alloc::{GlobalAlloc, Layout, System},
    cell::Cell,
    fmt,
    os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd},
    sync::{
        Mutex, OnceLock,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

/// Bytes per slab slot. One slot holds one page plus its tag-sized tail rounding.
pub const SLOT_BYTES: usize = 16 * 1024 * 1024;

/// Largest alignment a slot satisfies. Slots start on page boundaries.
const SLOT_ALIGN: usize = 4096;

/// First byte of the slab mapping, or zero before [`install`].
static BASE: AtomicUsize = AtomicUsize::new(0);

/// One past the last byte of the slab mapping, or zero before [`install`].
static END: AtomicUsize = AtomicUsize::new(0);

/// Set once the global allocator has observed a request, proving it is active.
static PROBED: AtomicBool = AtomicBool::new(false);

/// Slot bookkeeping. Capacity is reserved at install so the allocator never
/// allocates while holding this lock.
static SLOTS: Mutex<Slots> = Mutex::new(Slots {
    free: Vec::new(),
    dirty: Vec::new(),
    per_node: 0,
});

/// Slab partition for each CPU, set at install. Empty means one partition.
static CPU_PARTITION: OnceLock<Vec<u16>> = OnceLock::new();

/// Descriptors and size for the installed slab.
static MEMORY: OnceLock<Memory> = OnceLock::new();

thread_local! {
    /// Routes the next allocations on this thread into the slab.
    static REQUEST: Cell<bool> = const { Cell::new(false) };
}

struct Slots {
    /// Free slot indices per NUMA partition, used as stacks so recently used
    /// slots stay warm. Partition `p` owns slots `p * per_node ..`, and the last
    /// partition also owns the remainder.
    free: Vec<Vec<u32>>,
    /// Whether a slot may hold nonzero bytes. Fresh memfd slots read as zero.
    dirty: Vec<bool>,
    /// Slots per partition.
    per_node: usize,
}

impl Slots {
    fn partition(&self, index: usize) -> usize {
        (index / self.per_node.max(1)).min(self.free.len().saturating_sub(1))
    }
}

struct Memory {
    /// Read-only descriptor handed to clients.
    readonly: OwnedFd,
    /// Writable memfd, kept so the mapping's backing file stays alive and sealed.
    _writable: OwnedFd,
    /// Mapped bytes.
    len: usize,
}

/// Failures installing the slab.
#[derive(Debug)]
#[non_exhaustive]
pub enum InstallError {
    /// The binary did not declare [`SharedAllocator`] as its global allocator.
    AllocatorInactive,
    /// The slab was already installed.
    AlreadyInstalled,
    /// Requested size is zero or does not fit the slot index.
    InvalidSize,
    /// A kernel call failed.
    Os(&'static str, std::io::Error),
}

impl fmt::Display for InstallError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::AllocatorInactive => write!(f, "shared allocator is not the global allocator"),
            Self::AlreadyInstalled => write!(f, "shared memory already installed"),
            Self::InvalidSize => write!(f, "invalid shared memory size"),
            Self::Os(call, error) => write!(f, "{call}: {error}"),
        }
    }
}

impl std::error::Error for InstallError {}

/// Global allocator that serves requested allocations from the shared slab.
///
/// Declare it with `#[global_allocator]` in a binary that calls [`install`].
#[derive(Debug, Default)]
pub struct SharedAllocator;

impl SharedAllocator {
    /// Create the allocator; usable in a `static`.
    pub const fn new() -> Self {
        Self
    }
}

fn requested() -> bool {
    REQUEST.with(Cell::get)
}

fn slab_index(pointer: *mut u8) -> Option<usize> {
    let address = pointer as usize;
    let base = BASE.load(Ordering::Acquire);
    if base == 0 || address < base || address >= END.load(Ordering::Acquire) {
        return None;
    }
    Some((address - base) / SLOT_BYTES)
}

/// Take a slot, returning its pointer and whether it may hold nonzero bytes.
fn take_slot(layout: Layout) -> Option<(*mut u8, bool)> {
    if layout.size() > SLOT_BYTES || layout.align() > SLOT_ALIGN {
        return None;
    }
    let base = BASE.load(Ordering::Acquire);
    if base == 0 {
        return None;
    }
    let local = local_partition();
    let mut slots = SLOTS.lock().unwrap_or_else(|e| e.into_inner());
    let partitions = slots.free.len();
    let index =
        (0..partitions).find_map(|step| slots.free[(local + step) % partitions].pop())? as usize;
    let dirty = std::mem::replace(&mut slots.dirty[index], true);
    Some(((base + index * SLOT_BYTES) as *mut u8, dirty))
}

/// Partition of the NUMA node running this thread. Dataplane threads are pinned,
/// so the slot stays local to the thread that fills and later reads it.
fn local_partition() -> usize {
    let Some(table) = CPU_PARTITION.get() else {
        return 0;
    };
    // SAFETY: sched_getcpu has no preconditions.
    let cpu = unsafe { libc::sched_getcpu() };
    usize::try_from(cpu)
        .ok()
        .and_then(|cpu| table.get(cpu))
        .map_or(0, |&partition| usize::from(partition))
}

fn release_slot(index: usize) {
    let mut slots = SLOTS.lock().unwrap_or_else(|e| e.into_inner());
    let partition = slots.partition(index);
    let free = &mut slots.free[partition];
    // Capacity was reserved for every slot at install; this never allocates.
    debug_assert!(free.len() < free.capacity());
    free.push(index as u32);
}

// SAFETY: slab slots are disjoint, page-aligned ranges of a live mapping that is
// never unmapped, handed to one owner at a time under the slot lock. Every other
// request is forwarded to `System` with the same layout.
unsafe impl GlobalAlloc for SharedAllocator {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        if requested() {
            PROBED.store(true, Ordering::Relaxed);
            if let Some((pointer, _)) = take_slot(layout) {
                return pointer;
            }
        }
        // SAFETY: forwarded with the caller's layout contract.
        unsafe { System.alloc(layout) }
    }

    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        if requested() {
            PROBED.store(true, Ordering::Relaxed);
            if let Some((pointer, dirty)) = take_slot(layout) {
                if dirty {
                    // SAFETY: the slot is exclusively ours and at least layout.size() long.
                    unsafe { std::ptr::write_bytes(pointer, 0, layout.size()) };
                }
                return pointer;
            }
        }
        // SAFETY: forwarded with the caller's layout contract.
        unsafe { System.alloc_zeroed(layout) }
    }

    unsafe fn dealloc(&self, pointer: *mut u8, layout: Layout) {
        match slab_index(pointer) {
            Some(index) => release_slot(index),
            // SAFETY: not a slab pointer, so System allocated it with this layout.
            None => unsafe { System.dealloc(pointer, layout) },
        }
    }

    unsafe fn realloc(&self, pointer: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        let Some(index) = slab_index(pointer) else {
            // SAFETY: not a slab pointer, so System allocated it with this layout.
            return unsafe { System.realloc(pointer, layout, new_size) };
        };
        if new_size <= SLOT_BYTES {
            // The slot already spans SLOT_BYTES; growing or shrinking is free.
            return pointer;
        }
        // SAFETY: new_size is nonzero and the alignment came from a valid layout.
        let layout_new = unsafe { Layout::from_size_align_unchecked(new_size, layout.align()) };
        // SAFETY: forwarded with a valid nonzero layout.
        let moved = unsafe { System.alloc(layout_new) };
        if !moved.is_null() {
            // SAFETY: both ranges are valid for layout.size() bytes and disjoint.
            unsafe { std::ptr::copy_nonoverlapping(pointer, moved, layout.size()) };
            release_slot(index);
        }
        moved
    }
}

/// Run `allocate` with slab allocation requested on this thread.
fn with_request<T>(allocate: impl FnOnce() -> T) -> T {
    struct Reset(bool);
    impl Drop for Reset {
        fn drop(&mut self) {
            REQUEST.with(|r| r.set(self.0));
        }
    }
    let _reset = Reset(REQUEST.with(|r| r.replace(true)));
    allocate()
}

/// Map a sealed memfd of at least `bytes` and serve requested allocations from it.
///
/// Fails unless [`SharedAllocator`] is the global allocator. Installs at most once.
pub fn install(bytes: usize) -> Result<(), InstallError> {
    if MEMORY.get().is_some() {
        return Err(InstallError::AlreadyInstalled);
    }
    // Prove the global allocator routes through SharedAllocator.
    drop(with_request(|| std::hint::black_box(Box::new(0u8))));
    if !PROBED.load(Ordering::Relaxed) {
        return Err(InstallError::AllocatorInactive);
    }
    let count = bytes.div_ceil(SLOT_BYTES);
    if count == 0 || count > u32::MAX as usize {
        return Err(InstallError::InvalidSize);
    }
    let len = count
        .checked_mul(SLOT_BYTES)
        .ok_or(InstallError::InvalidSize)?;
    let os = |call| move |error| InstallError::Os(call, error);

    // SAFETY: the name is a valid NUL-terminated string.
    let raw = unsafe {
        libc::memfd_create(
            c"racer-plaintext".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    if raw < 0 {
        return Err(os("memfd_create")(std::io::Error::last_os_error()));
    }
    // SAFETY: memfd_create returned a fresh descriptor we own.
    let writable = unsafe { OwnedFd::from_raw_fd(raw) };
    // SAFETY: valid descriptor; length checked to fit off_t below.
    let length = libc::off_t::try_from(len).map_err(|_| InstallError::InvalidSize)?;
    if unsafe { libc::ftruncate(writable.as_raw_fd(), length) } != 0 {
        return Err(os("ftruncate")(std::io::Error::last_os_error()));
    }
    // SAFETY: maps the whole file shared and writable; never unmapped.
    let base = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_SHARED,
            writable.as_raw_fd(),
            0,
        )
    };
    if base == libc::MAP_FAILED {
        return Err(os("mmap")(std::io::Error::last_os_error()));
    }
    // Best effort: shmem huge pages depend on host configuration.
    // SAFETY: advises the mapping created above.
    unsafe { libc::madvise(base, len, libc::MADV_HUGEPAGE) };

    // Reopen read-only through procfs so clients cannot map it writable.
    let path = format!("/proc/self/fd/{}\0", writable.as_raw_fd());
    // SAFETY: path is NUL-terminated.
    let raw = unsafe { libc::open(path.as_ptr().cast(), libc::O_RDONLY | libc::O_CLOEXEC) };
    if raw < 0 {
        let error = std::io::Error::last_os_error();
        // SAFETY: unmaps the mapping created above, which no one references yet.
        unsafe { libc::munmap(base, len) };
        return Err(os("open readonly memfd")(error));
    }
    // SAFETY: open returned a fresh descriptor we own.
    let readonly = unsafe { OwnedFd::from_raw_fd(raw) };
    // Freeze size and forbid new writable mappings. Our existing mapping stays writable.
    let seals =
        libc::F_SEAL_SHRINK | libc::F_SEAL_GROW | libc::F_SEAL_FUTURE_WRITE | libc::F_SEAL_SEAL;
    // SAFETY: valid memfd created with MFD_ALLOW_SEALING.
    if unsafe { libc::fcntl(writable.as_raw_fd(), libc::F_ADD_SEALS, seals) } != 0 {
        let error = std::io::Error::last_os_error();
        // SAFETY: unmaps the mapping created above, which no one references yet.
        unsafe { libc::munmap(base, len) };
        return Err(os("seal memfd")(error));
    }

    // Split slots across NUMA nodes so each thread fills memory on its own node.
    let nodes = numa_nodes();
    let partitions = if nodes.len() > 1 && count >= nodes.len() {
        nodes.len()
    } else {
        1
    };
    let per_node = count / partitions;
    let mut table = Vec::new();
    if partitions > 1 {
        for (partition, (node, cpus)) in nodes.iter().enumerate() {
            let first = partition * per_node;
            let slots = if partition + 1 == partitions {
                count - first
            } else {
                per_node
            };
            // SAFETY: the range lies inside the mapping created above.
            bind_node(
                unsafe { base.cast::<u8>().add(first * SLOT_BYTES) },
                slots * SLOT_BYTES,
                *node,
            );
            for &cpu in cpus {
                if table.len() <= cpu {
                    table.resize(cpu + 1, 0);
                }
                table[cpu] = partition as u16;
            }
        }
    }
    {
        let mut slots = SLOTS.lock().unwrap_or_else(|e| e.into_inner());
        slots.per_node = per_node;
        slots.free = (0..partitions)
            .map(|partition| {
                let first = partition * per_node;
                let last = if partition + 1 == partitions {
                    count
                } else {
                    first + per_node
                };
                let mut free = Vec::with_capacity(last - first);
                free.extend((first as u32..last as u32).rev());
                free
            })
            .collect();
        slots.dirty = vec![false; count];
    }
    if partitions > 1 {
        let _ = CPU_PARTITION.set(table);
    }
    if MEMORY
        .set(Memory {
            readonly,
            _writable: writable,
            len,
        })
        .is_err()
    {
        // SAFETY: unmaps the mapping created above, which no one references yet.
        unsafe { libc::munmap(base, len) };
        return Err(InstallError::AlreadyInstalled);
    }
    END.store(base as usize + len, Ordering::Release);
    BASE.store(base as usize, Ordering::Release);
    Ok(())
}

/// Online NUMA nodes and their CPUs, from sysfs. Empty when unknown.
fn numa_nodes() -> Vec<(usize, Vec<usize>)> {
    let Ok(online) = std::fs::read_to_string("/sys/devices/system/node/online") else {
        return Vec::new();
    };
    parse_list(&online)
        .into_iter()
        .filter_map(|node| {
            let path = format!("/sys/devices/system/node/node{node}/cpulist");
            let cpus = parse_list(&std::fs::read_to_string(path).ok()?);
            (!cpus.is_empty()).then_some((node, cpus))
        })
        .collect()
}

/// Parse a kernel list such as `0-63,128-191`. Malformed parts are skipped.
fn parse_list(text: &str) -> Vec<usize> {
    let mut values = Vec::new();
    for part in text.trim().split(',').filter(|part| !part.is_empty()) {
        let (first, last) = part.split_once('-').unwrap_or((part, part));
        if let (Ok(first), Ok(last)) = (first.parse::<usize>(), last.parse::<usize>())
            && first <= last
            && last < 1 << 16
        {
            values.extend(first..=last);
        }
    }
    values
}

/// Prefer `node` for pages of `[start, start + len)`. Best effort: without the
/// policy, pages land on whichever node first touches them.
fn bind_node(start: *mut u8, len: usize, node: usize) {
    const MPOL_PREFERRED: libc::c_long = 1;
    let mut mask = [0u64; 16];
    let Some(word) = mask.get_mut(node / 64) else {
        return;
    };
    *word |= 1 << (node % 64);
    // SAFETY: the range is a live mapping and the mask outlives the call.
    unsafe {
        libc::syscall(
            libc::SYS_mbind,
            start,
            len,
            MPOL_PREFERRED,
            mask.as_ptr(),
            (mask.len() * 64) as libc::c_ulong,
            0 as libc::c_uint,
        );
    }
}

/// Whether the slab is installed.
pub fn installed() -> bool {
    BASE.load(Ordering::Acquire) != 0
}

/// Read-only descriptor for the slab, if installed.
pub fn readonly_fd() -> Option<BorrowedFd<'static>> {
    MEMORY.get().map(|memory| memory.readonly.as_fd())
}

/// Mapped slab size in bytes, or zero before [`install`].
pub fn len() -> usize {
    MEMORY.get().map_or(0, |memory| memory.len)
}

/// Number of free slots, or zero before [`install`].
pub fn free_slots() -> usize {
    SLOTS
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .free
        .iter()
        .map(Vec::len)
        .sum()
}

/// Whether `pointer` lies inside the slab.
pub fn contains(pointer: *const u8) -> bool {
    slab_index(pointer.cast_mut()).is_some()
}

/// Slab offset of `bytes` when the whole range lies inside one slot.
pub fn offset_of(bytes: &[u8]) -> Option<u64> {
    let start = bytes.as_ptr() as usize;
    let base = BASE.load(Ordering::Acquire);
    if base == 0 || start < base {
        return None;
    }
    let offset = start - base;
    let end = offset.checked_add(bytes.len())?;
    let slot = offset / SLOT_BYTES;
    // Empty ranges at a slot boundary still belong to that slot.
    if end > (slot + 1) * SLOT_BYTES || base + end > END.load(Ordering::Acquire) {
        return None;
    }
    u64::try_from(offset).ok()
}

/// Zeroed slab-backed bytes of `length`, or `None` when unavailable or full.
pub fn zeroed_vec(length: usize) -> Option<Vec<u8>> {
    if length == 0 || length > SLOT_BYTES || !installed() {
        return None;
    }
    let layout = Layout::from_size_align(length, 1).ok()?;
    // SAFETY: nonzero layout; the returned pointer is checked below.
    let pointer = with_request(|| unsafe { std::alloc::alloc_zeroed(layout) });
    if pointer.is_null() {
        return None;
    }
    if !contains(pointer) {
        // Slab full: do not hand out private memory as shared.
        // SAFETY: allocated above with this layout.
        unsafe { std::alloc::dealloc(pointer, layout) };
        return None;
    }
    // SAFETY: zeroed allocation of exactly `length` bytes with Vec<u8>'s layout.
    Some(unsafe { Vec::from_raw_parts(pointer, length, length) })
}

/// Empty slab-backed bytes with `capacity`, or `None` when unavailable or full.
pub fn vec_with_capacity(capacity: usize) -> Option<Vec<u8>> {
    if capacity == 0 || capacity > SLOT_BYTES || !installed() {
        return None;
    }
    let bytes = with_request(|| Vec::<u8>::with_capacity(capacity));
    contains(bytes.as_ptr()).then_some(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inactive_allocator_refuses_install_and_serves_nothing() {
        // Unit tests run under the default global allocator.
        assert!(matches!(
            install(SLOT_BYTES),
            Err(InstallError::AllocatorInactive)
        ));
        assert!(!installed());
        assert!(zeroed_vec(1024).is_none());
        assert!(vec_with_capacity(1024).is_none());
        assert!(readonly_fd().is_none());
        assert!(offset_of(&[0u8; 4]).is_none());
        assert!(!contains(std::ptr::null()));
    }

    #[test]
    fn kernel_lists_parse_ranges_and_skip_garbage() {
        assert_eq!(parse_list("0-2,5\n"), vec![0, 1, 2, 5]);
        assert_eq!(parse_list("3"), vec![3]);
        assert_eq!(parse_list(""), Vec::<usize>::new());
        assert_eq!(parse_list("4-2,x,7-8"), vec![7, 8]);
    }

    #[test]
    fn last_partition_owns_the_remainder() {
        let slots = Slots {
            free: vec![Vec::new(), Vec::new()],
            dirty: Vec::new(),
            per_node: 2,
        };
        let partitions: Vec<_> = (0..5).map(|index| slots.partition(index)).collect();
        assert_eq!(partitions, vec![0, 0, 1, 1, 1]);
    }
}
