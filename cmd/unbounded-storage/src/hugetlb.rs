// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use crate::bufferpool::{Backing, Error};

#[cfg(target_os = "linux")]
const PAGE_SIZE_2MB: usize = 2 * 1024 * 1024;

/// Allocate `page_count` 2 MiB hugepages and wrap them in a
/// [`Backing`]. Fails fast if the host has not reserved enough
/// hugepages on the calling NUMA domain (via `MADV_POPULATE_WRITE`,
/// Linux 5.14+).
///
/// `MADV_DONTFORK` is applied to the backing on the way out so the
/// post-fork CoW hazard is neutralised before any RDMA MR registers
/// the range. NUMA pinning of the calling thread (e.g.
/// `sched_setaffinity` or `MPOL_BIND` on the VMA) remains the
/// embedder's responsibility.
#[cfg(target_os = "linux")]
pub fn allocate_2mb_backing(page_count: usize) -> Result<Backing, Error> {
    allocate_with(PAGE_SIZE_2MB, page_count, libc::MAP_HUGE_2MB)
}

#[cfg(not(target_os = "linux"))]
pub fn allocate_2mb_backing(_page_count: usize) -> Result<Backing, Error> {
    Err(Error::BadConfig("hugepages only available on linux"))
}

#[cfg(target_os = "linux")]
fn allocate_with(page_size: usize, page_count: usize, huge_flag: i32) -> Result<Backing, Error> {
    if page_count == 0 {
        return Err(Error::BadConfig("hugepage count must be > 0"));
    }
    let len = page_size
        .checked_mul(page_count)
        .ok_or(Error::BadConfig("hugepage allocation overflow"))?;

    // SAFETY: arguments are valid; we check the return value.
    let ptr = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_HUGETLB | huge_flag,
            -1,
            0,
        )
    };
    if ptr == libc::MAP_FAILED {
        return Err(Error::Io(io_errno()));
    }

    let owner = HugepageOwner {
        base: ptr as *mut u8,
        len,
    };

    // Fail-fast on hugepage shortage. `MAP_POPULATE` is not
    // sufficient on hugetlb mappings (mmap(2) explicitly notes the
    // call does not fail when the pool is exhausted; the caller
    // would later trip a SIGBUS at first touch). MADV_POPULATE_WRITE
    // (Linux 5.14+, mm/madvise.c) walks the range now and surfaces
    // the shortage as a syscall error. See designs/bufferpool.md
    // "Constraints" point 2.
    //
    // SAFETY: `owner.base` is the live mmap returned above; `len`
    // matches; the call only faults pages, never invalidates the
    // mapping.
    let rc = unsafe {
        libc::madvise(
            owner.base as *mut libc::c_void,
            len,
            libc::MADV_POPULATE_WRITE,
        )
    };
    if rc != 0 {
        return Err(Error::Io(io_errno()));
    }

    // Neutralise the post-fork CoW hazard before any RDMA MR
    // registers the range. MADV_DONTFORK detaches the mapping from
    // child address spaces; libibverbs' alternative is
    // ibv_fork_init() / RDMAV_FORK_SAFE which apply the same hint
    // internally. We apply it unconditionally so embedders that do
    // not pull in libibverbs still get the guarantee. See
    // designs/bufferpool.md "Constraints" point 3.
    //
    // SAFETY: same as above. MADV_DONTFORK does not change current
    // process page tables; only fork()'s behaviour for this VMA.
    let rc = unsafe { libc::madvise(owner.base as *mut libc::c_void, len, libc::MADV_DONTFORK) };
    if rc != 0 {
        return Err(Error::Io(io_errno()));
    }

    Ok(Backing {
        base: owner.base,
        page_size,
        page_count,
        _own: Box::new(owner),
    })
}

#[cfg(target_os = "linux")]
fn io_errno() -> i32 {
    std::io::Error::last_os_error().raw_os_error().unwrap_or(0)
}

/// Drop carrier for the mmap'd region. Holds the bookkeeping the
/// pool itself never inspects (per the design's `Backing._own`
/// contract).
#[cfg(target_os = "linux")]
struct HugepageOwner {
    base: *mut u8,
    len: usize,
}

// SAFETY: hugepages are pinned by the kernel and the mapping lives
// for the lifetime of the owner. The owner is sent only as part of
// `Backing` whose `Send + Sync` impls already document the
// per-NUMA pinning invariant.
#[cfg(target_os = "linux")]
unsafe impl Send for HugepageOwner {}
#[cfg(target_os = "linux")]
unsafe impl Sync for HugepageOwner {}

#[cfg(target_os = "linux")]
impl Drop for HugepageOwner {
    fn drop(&mut self) {
        // SAFETY: matches the `mmap` above.
        unsafe {
            libc::munmap(self.base as *mut libc::c_void, self.len);
        }
    }
}
