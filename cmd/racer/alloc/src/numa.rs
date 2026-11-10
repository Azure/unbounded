//! NUMA memory policy for pooled storage.

/// `MPOL_PREFERRED` from `linux/mempolicy.h`.
const MPOL_PREFERRED: libc::c_int = 1;

/// Bits in one node mask word.
const WORD_BITS: usize = libc::c_ulong::BITS as usize;

/// Ask the kernel to place the pages of `[pointer, pointer + length)` on
/// `node` when they are first touched, falling back to other nodes when it is
/// full. Pages already present do not move. Returns whether the kernel took
/// the policy; it refuses a node it does not have, and seccomp may deny
/// `mbind`.
pub(crate) fn prefer_node(pointer: *mut u8, length: usize, node: u16) -> bool {
    let node = usize::from(node);
    let mut mask = vec![0 as libc::c_ulong; node / WORD_BITS + 1];
    mask[node / WORD_BITS] = 1 << (node % WORD_BITS);
    // The kernel reads `maxnode - 1` bits; pass one more than the mask holds.
    let maxnode = mask.len() * WORD_BITS + 1;
    // SAFETY: the caller owns the page-aligned mapping; mbind changes only its
    // placement policy and reads `mask.len()` words of the live mask.
    let status = unsafe {
        libc::syscall(
            libc::SYS_mbind,
            pointer,
            length,
            MPOL_PREFERRED,
            mask.as_ptr(),
            maxnode,
            0 as libc::c_uint,
        )
    };
    status == 0
}
