// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::fmt;

use unbounded_storage::bufferpool::Backing;

/// 2 MiB hugepage size in bytes. Hard-coded; 1 GiB hugepages are
/// intentionally out of scope.
pub const HUGEPAGE_2MB: usize = 2 * 1024 * 1024;

/// Which allocator to use for a shard's backing.
#[derive(Copy, Clone, Debug)]
pub enum BackingKind {
    /// `mmap(MAP_ANONYMOUS | MAP_HUGETLB | MAP_HUGE_2MB)`. Fails
    /// hard if the kernel's 2 MiB hugepage pool cannot satisfy the
    /// request; there is no automatic fallback.
    Hugepage2Mb,
    /// `std::alloc::alloc_zeroed` with 2 MiB alignment. Use only on
    /// hosts where reserving hugepages is not feasible (CI, dev).
    Heap,
}

/// Request to allocate a shard backing. `bytes` is rounded up to a
/// multiple of the chosen page size; the resulting `Backing` may
/// therefore be larger than requested.
#[derive(Copy, Clone, Debug)]
pub struct BackingRequest {
    pub kind: BackingKind,
    pub bytes: usize,
    pub numa: Option<u16>,
}

#[derive(Debug)]
pub enum BackingError {
    /// `mmap` for hugepages failed. `free_hugepages` is the value
    /// read from `/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages`
    /// at failure time (or `None` if that file could not be read);
    /// surfacing it inline saves the operator a second hop.
    HugepageMmap {
        errno: i32,
        free_hugepages: Option<u64>,
    },
    /// `mbind` failed after a successful `mmap`. The mapping is
    /// already unmapped by the time this error is returned.
    NumaBind { errno: i32 },
    /// Heap allocator returned null for the requested layout.
    HeapAlloc,
    /// `bytes` was zero, or rounding overflowed `usize`.
    InvalidSize,
}

impl fmt::Display for BackingError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            BackingError::HugepageMmap {
                errno,
                free_hugepages,
            } => {
                let err = std::io::Error::from_raw_os_error(*errno);
                match free_hugepages {
                    Some(n) => write!(f, "hugepage mmap failed: {err} (free 2MiB hugepages={n})"),
                    None => write!(f, "hugepage mmap failed: {err}"),
                }
            }
            BackingError::NumaBind { errno } => {
                let err = std::io::Error::from_raw_os_error(*errno);
                write!(f, "mbind failed: {err}")
            }
            BackingError::HeapAlloc => write!(f, "heap allocation returned null"),
            BackingError::InvalidSize => write!(f, "invalid backing size"),
        }
    }
}

impl std::error::Error for BackingError {}

/// Allocate a `Backing` per `req`. The page size used to populate
/// `Backing::page_size` is the allocator's native page size
/// (2 MiB for both current variants).
pub fn allocate(req: BackingRequest) -> Result<Backing, BackingError> {
    let page_size = HUGEPAGE_2MB;
    let size = round_up_to(req.bytes, page_size).ok_or(BackingError::InvalidSize)?;
    if size == 0 {
        return Err(BackingError::InvalidSize);
    }
    let page_count = size / page_size;
    match req.kind {
        BackingKind::Hugepage2Mb => allocate_hugepage(size, page_size, page_count, req.numa),
        BackingKind::Heap => allocate_heap(size, page_size, page_count),
    }
}

/// Round `bytes` up to the next multiple of `page_size`. Returns
/// `None` on overflow. Pure; exposed for testing.
fn round_up_to(bytes: usize, page_size: usize) -> Option<usize> {
    if page_size == 0 {
        return None;
    }
    let rem = bytes % page_size;
    if rem == 0 {
        Some(bytes)
    } else {
        bytes.checked_add(page_size - rem)
    }
}

fn allocate_hugepage(
    size: usize,
    page_size: usize,
    page_count: usize,
    numa: Option<u16>,
) -> Result<Backing, BackingError> {
    // SAFETY: standard anonymous mmap. We check for MAP_FAILED and
    // never dereference the returned pointer here.
    let ptr = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_HUGETLB | libc::MAP_HUGE_2MB,
            -1,
            0,
        )
    };
    if ptr == libc::MAP_FAILED {
        let errno = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
        return Err(BackingError::HugepageMmap {
            errno,
            free_hugepages: read_free_hugepages_2mb(),
        });
    }

    if let Some(node) = numa {
        if let Err(errno) = mbind_to_node(ptr, size, node) {
            // SAFETY: ptr/size came from the mmap above; munmap is
            // the matching teardown.
            unsafe {
                libc::munmap(ptr, size);
            }
            return Err(BackingError::NumaBind { errno });
        }
    }

    Ok(Backing {
        base: ptr as *mut u8,
        page_size,
        page_count,
        _own: Box::new(HugepageOwner { ptr, size }),
    })
}

fn mbind_to_node(ptr: *mut libc::c_void, size: usize, node: u16) -> Result<(), i32> {
    // MPOL_BIND: strictly allocate from the named node. Stronger
    // than the `MPOL_PREFERRED` used by the per-thread runtime
    // policy because hugepages are reserved up front and we want a
    // hard failure if the reservation cannot be satisfied locally.
    const MPOL_BIND: libc::c_int = 2;
    const MPOL_MF_STRICT: libc::c_uint = 1;

    let bits_per_word = std::mem::size_of::<libc::c_ulong>() * 8;
    let bit = node as usize;
    let words = bit / bits_per_word + 1;
    let mut mask = vec![0 as libc::c_ulong; words];
    mask[bit / bits_per_word] |= (1 as libc::c_ulong) << (bit % bits_per_word);
    let maxnode = (words * bits_per_word) as libc::c_ulong;

    // SAFETY: mask is a valid bitmask sized at `maxnode` bits.
    let rc = unsafe {
        libc::syscall(
            libc::SYS_mbind,
            ptr as libc::c_long,
            size as libc::c_long,
            MPOL_BIND as libc::c_long,
            mask.as_ptr() as libc::c_long,
            maxnode as libc::c_long,
            MPOL_MF_STRICT as libc::c_long,
        )
    };
    if rc != 0 {
        return Err(std::io::Error::last_os_error().raw_os_error().unwrap_or(0));
    }
    Ok(())
}

/// Best-effort read of the free 2 MiB hugepage count. The path is
/// the kernel's canonical sysfs entry; we return `None` if it is
/// unreadable rather than masking the original mmap failure.
fn read_free_hugepages_2mb() -> Option<u64> {
    let s =
        std::fs::read_to_string("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages").ok()?;
    s.trim().parse::<u64>().ok()
}

fn allocate_heap(
    size: usize,
    page_size: usize,
    page_count: usize,
) -> Result<Backing, BackingError> {
    let layout = std::alloc::Layout::from_size_align(size, page_size)
        .map_err(|_| BackingError::InvalidSize)?;
    // SAFETY: layout has nonzero size (checked by `allocate`) and a
    // power-of-two alignment (page_size is 2 MiB).
    let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
    if ptr.is_null() {
        return Err(BackingError::HeapAlloc);
    }
    Ok(Backing {
        base: ptr,
        page_size,
        page_count,
        _own: Box::new(HeapOwner { ptr, layout }),
    })
}

/// Drop carrier for a `mmap`-backed hugepage region.
struct HugepageOwner {
    ptr: *mut libc::c_void,
    size: usize,
}

// SAFETY: the owner is moved into `Backing._own`; the mapping is
// only accessed through `Backing::base` whose synchronization is
// upheld by the `Pool` invariants.
unsafe impl Send for HugepageOwner {}
unsafe impl Sync for HugepageOwner {}

impl Drop for HugepageOwner {
    fn drop(&mut self) {
        // SAFETY: ptr/size are the values returned from mmap; no
        // other reference into the region remains.
        unsafe {
            libc::munmap(self.ptr, self.size);
        }
    }
}

/// Drop carrier for a heap-allocated region.
struct HeapOwner {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

// SAFETY: see `HugepageOwner`. The allocation lives as long as the
// pool that references it.
unsafe impl Send for HeapOwner {}
unsafe impl Sync for HeapOwner {}

impl Drop for HeapOwner {
    fn drop(&mut self) {
        // SAFETY: matches the `alloc_zeroed` call in `allocate_heap`.
        unsafe {
            std::alloc::dealloc(self.ptr, self.layout);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_up_already_aligned() {
        assert_eq!(round_up_to(HUGEPAGE_2MB, HUGEPAGE_2MB), Some(HUGEPAGE_2MB));
        assert_eq!(
            round_up_to(4 * HUGEPAGE_2MB, HUGEPAGE_2MB),
            Some(4 * HUGEPAGE_2MB)
        );
    }

    #[test]
    fn round_up_rounds_partial() {
        assert_eq!(round_up_to(1, HUGEPAGE_2MB), Some(HUGEPAGE_2MB));
        assert_eq!(
            round_up_to(HUGEPAGE_2MB + 1, HUGEPAGE_2MB),
            Some(2 * HUGEPAGE_2MB),
        );
    }

    #[test]
    fn round_up_zero() {
        assert_eq!(round_up_to(0, HUGEPAGE_2MB), Some(0));
    }

    #[test]
    fn round_up_overflow_returns_none() {
        assert_eq!(round_up_to(usize::MAX, HUGEPAGE_2MB), None);
    }

    #[test]
    fn heap_backing_alloc_and_drop() {
        let b = allocate(BackingRequest {
            kind: BackingKind::Heap,
            bytes: HUGEPAGE_2MB,
            numa: None,
        })
        .expect("heap alloc");
        assert_eq!(b.page_size, HUGEPAGE_2MB);
        assert_eq!(b.page_count, 1);
        assert!(!b.base.is_null());
        // Touch the first and last byte to confirm the mapping is
        // writable; alloc_zeroed promises zero-init so reads should
        // return 0.
        // SAFETY: bytes inside the allocation.
        unsafe {
            assert_eq!(*b.base, 0);
            *b.base = 0xAB;
            let last = b.base.add(HUGEPAGE_2MB - 1);
            assert_eq!(*last, 0);
            *last = 0xCD;
        }
        drop(b);
    }

    #[test]
    fn heap_rounds_up_to_page() {
        let b = allocate(BackingRequest {
            kind: BackingKind::Heap,
            bytes: 1,
            numa: None,
        })
        .expect("heap alloc");
        assert_eq!(b.page_size, HUGEPAGE_2MB);
        assert_eq!(b.page_count, 1);
    }

    #[test]
    fn zero_bytes_is_invalid() {
        let err = allocate(BackingRequest {
            kind: BackingKind::Heap,
            bytes: 0,
            numa: None,
        })
        .err()
        .expect("zero must reject");
        assert!(matches!(err, BackingError::InvalidSize));
    }

    // Hugepage path requires reserved 2 MiB pages on the host;
    // opt-in via `UB_STORAGE_HUGEPAGE_TEST=1` so CI without
    // reservations does not flap.
    #[test]
    fn hugepage_backing_when_enabled() {
        if std::env::var_os("UB_STORAGE_HUGEPAGE_TEST").is_none() {
            return;
        }
        let b = allocate(BackingRequest {
            kind: BackingKind::Hugepage2Mb,
            bytes: HUGEPAGE_2MB,
            numa: None,
        })
        .expect("hugepage alloc with UB_STORAGE_HUGEPAGE_TEST=1");
        assert_eq!(b.page_size, HUGEPAGE_2MB);
        assert_eq!(b.page_count, 1);
        // SAFETY: bytes inside the mapping.
        unsafe {
            *b.base = 0xAB;
        }
    }
}
