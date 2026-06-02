// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! NUMA memory-binding syscalls used when allocating backings.
//!
//! This file owns the one `mbind` path the storage daemon uses to pin
//! a freshly-mapped region to a specific NUMA node.
//!
//! NUMA policy split (intentional, do not unify): shard *threads* are
//! placed with a soft `MPOL_PREFERRED` policy (see
//! `runtime/pinned.rs::set_preferred_node`) so that if the preferred
//! node is momentarily out of free pages the allocation can still fall
//! back to another node rather than failing - threads must keep
//! running. Backing *buffers*, in contrast, are pinned hard with
//! `MPOL_BIND | MPOL_MF_STRICT`: they are registered with the
//! NIC/io_uring and handed out as long-lived DMA targets, so they must
//! never migrate to another node. A hard failure at reservation time
//! is preferable to silent cross-node DMA later.

/// Bind the mapping at `ptr` (covering `size` bytes) hard to NUMA
/// `node` via `mbind(MPOL_BIND | MPOL_MF_STRICT)`. Returns the errno
/// on failure. See the module docs for why this is a hard bind while
/// thread placement is only a soft preference.
pub(crate) fn mbind_to_node(ptr: *mut libc::c_void, size: usize, node: u16) -> Result<(), i32> {
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
