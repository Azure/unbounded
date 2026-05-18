// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Wire schema for the single `ub.bufferpool.bulk_get.v1` RPC.
//!
//! The Rust side stages input/output through plain `#[repr(C)]`
//! structs that mirror `shim.c` byte-for-byte. The C shim provides
//! the two proc callbacks Mercury needs to walk these structs on
//! encode / decode; we never poke at proc internals from Rust.
//!
//! The opaque `req_bytes` slice carries the bincode-encoded
//! `R: Serialize`. Routing metadata (`stripe_key`, `stripe_off`,
//! `len`) is duplicated on the wire so the server can dispatch a
//! request without deserializing `R` if its source happens not to
//! need it.

use crate::mercury::ffi;

/// Single registered RPC name. Bumping `v1` is the migration path
/// if the wire ever needs to change.
pub const RPC_NAME: &[u8] = b"ub.bufferpool.bulk_get.v1\0";

/// Wire input. Field order and types match `struct ub_bulk_get_in`
/// in `shim.c`; do not reorder without updating the shim.
#[repr(C)]
pub struct BulkGetIn {
    pub stripe_key: [u8; 32],
    pub stripe_off: u64,
    pub dst_offset: u64,
    pub len: u32,
    pub req_bytes_len: u32,
    /// On the client side, points into a `Box<[u8]>` owned by the
    /// caller for the lifetime of the `HG_Forward` call. On the
    /// server side, allocated by `malloc` inside the shim and freed
    /// by `HG_Free_input` (which invokes the proc with `HG_FREE`).
    pub req_bytes: *mut u8,
    pub dst_bulk: ffi::hg_bulk_t,
}

impl BulkGetIn {
    pub fn zeroed() -> Self {
        Self {
            stripe_key: [0u8; 32],
            stripe_off: 0,
            dst_offset: 0,
            len: 0,
            req_bytes_len: 0,
            req_bytes: std::ptr::null_mut(),
            dst_bulk: std::ptr::null_mut(),
        }
    }
}

/// Wire output. Single status code; non-zero means the server-side
/// bulk push failed or the source rejected the request.
#[repr(C)]
pub struct BulkGetOut {
    pub status: i32,
}

impl BulkGetOut {
    pub fn zeroed() -> Self {
        Self { status: 0 }
    }
}

/// Defensive check: the Rust `#[repr(C)]` struct must match the C
/// shim's view of the same layout. Mismatch would corrupt every
/// in-flight RPC; bail loudly at construction time instead.
pub(crate) fn assert_layouts() -> Result<(), &'static str> {
    // SAFETY: shim symbols return a `size_t` and have no side effects.
    let in_c = unsafe { ffi::ub_sizeof_bulk_get_in() };
    if in_c != std::mem::size_of::<BulkGetIn>() {
        return Err("BulkGetIn size mismatches shim layout");
    }
    let out_c = unsafe { ffi::ub_sizeof_bulk_get_out() };
    if out_c != std::mem::size_of::<BulkGetOut>() {
        return Err("BulkGetOut size mismatches shim layout");
    }
    Ok(())
}

/// Re-export the shim proc callbacks at safe-ish types. Mercury
/// invokes these on its progress thread; we never call them
/// directly. The functions only touch their `data` pointer and the
/// proc state, neither of which has Rust-visible aliasing concerns.
pub(crate) fn in_proc() -> ffi::hg_proc_cb_t {
    ffi::ub_proc_bulk_get_in
}

pub(crate) fn out_proc() -> ffi::hg_proc_cb_t {
    ffi::ub_proc_bulk_get_out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wire_layout_matches_shim() {
        assert_layouts().expect("layout mismatch");
    }
}
