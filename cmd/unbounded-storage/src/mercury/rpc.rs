// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! On-the-wire RPC structs and context selector.
//!
//! `BulkGetIn` / `BulkGetOut` mirror the C structs declared in
//! `mercury/shim.c`; the wire format is owned by that file and these
//! Rust types must keep field order and types identical to the C
//! definitions. The inline tests below cross-check the layout via
//! `ub_sizeof_*` shims.

use super::ffi::hg_bulk_t;

/// Mirror of `struct ub_bulk_get_in` defined in `mercury/shim.c`.
/// Wire format is owned by `shim.c`; this struct must keep field order
/// and types identical to that definition.
#[repr(C)]
pub struct BulkGetIn {
    pub stripe_key: [u8; 32],
    pub stripe_off: u64,
    pub dst_offset: u64,
    pub len: u32,
    pub req_bytes_len: u32,
    pub req_bytes: *mut u8,
    pub dst_bulk: hg_bulk_t,
}

/// Mirror of `struct ub_bulk_get_out` defined in `mercury/shim.c`.
#[repr(C)]
pub struct BulkGetOut {
    pub status: i32,
}

impl BulkGetIn {
    /// Returns a fully-zeroed `BulkGetIn`. Useful as a starting point for
    /// either populating before `HG_Forward` or as the receiving struct
    /// passed to `HG_Get_input`.
    ///
    /// # Safety
    /// `req_bytes` and `dst_bulk` are zeroed; the caller must populate
    /// them before any FFI use.
    pub unsafe fn zeroed() -> Self {
        // SAFETY: `BulkGetIn` is `#[repr(C)]` and contains only POD-style
        // fields (integers, fixed array, raw pointers). An all-zero
        // bit pattern is a valid value for every field; the pointer
        // fields become null, which the caller must overwrite before
        // any FFI use as documented above.
        unsafe { std::mem::zeroed() }
    }
}

impl BulkGetOut {
    /// Returns `BulkGetOut { status: 0 }`.
    pub fn ok() -> Self {
        Self { status: 0 }
    }
}

/// Distributes inbound RPCs across the contexts of a single `Nic`.
#[derive(Debug, Clone, Copy)]
pub struct CtxSelector {
    pub contexts: u16,
}

impl CtxSelector {
    /// Construct. `contexts` must be >= 1.
    pub fn new(contexts: u16) -> Self {
        assert!(contexts >= 1, "CtxSelector requires at least one context");
        Self { contexts }
    }

    /// Map a hash to a context index in `[0, self.contexts)`.
    pub fn pick(&self, addr_hash: u64) -> u16 {
        // Modulo bias on a 64-bit hash is negligible for the small
        // `contexts` values we use (typically <= 64).
        (addr_hash % self.contexts as u64) as u16
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mercury::ffi::{ub_sizeof_bulk_get_in, ub_sizeof_bulk_get_out};
    use rand::{Rng, SeedableRng};
    use rand_chacha::ChaCha8Rng;

    #[test]
    fn layout_matches_c_shim() {
        // SAFETY: `ub_sizeof_*` are pure C functions that return
        // `sizeof(struct)` and have no preconditions.
        assert_eq!(std::mem::size_of::<BulkGetIn>(), unsafe {
            ub_sizeof_bulk_get_in()
        });
        // SAFETY: as above.
        assert_eq!(std::mem::size_of::<BulkGetOut>(), unsafe {
            ub_sizeof_bulk_get_out()
        });
    }

    #[test]
    fn bulk_get_in_zeroed_is_all_zero() {
        // SAFETY: `zeroed()` returns a valid all-zero `BulkGetIn`; we
        // only read its fields here, never dereference the null
        // pointers it contains.
        let v = unsafe { BulkGetIn::zeroed() };
        assert_eq!(v.stripe_key, [0u8; 32]);
        assert_eq!(v.stripe_off, 0);
        assert_eq!(v.dst_offset, 0);
        assert_eq!(v.len, 0);
        assert_eq!(v.req_bytes_len, 0);
        assert!(v.req_bytes.is_null());
        assert!(v.dst_bulk.is_null());
    }

    #[test]
    fn bulk_get_out_ok_is_zero() {
        assert_eq!(BulkGetOut::ok().status, 0);
    }

    #[test]
    fn pick_stays_in_range() {
        for &contexts in &[1u16, 2, 4, 8] {
            let sel = CtxSelector::new(contexts);
            // 1024 evenly spaced u64 values across the full range.
            let step = u64::MAX / 1024;
            for i in 0..1024u64 {
                let h = i.wrapping_mul(step);
                let idx = sel.pick(h);
                assert!(idx < contexts, "pick({h}) = {idx} not in [0, {contexts})");
            }
        }
    }

    #[test]
    fn pick_distribution_is_balanced() {
        let sel = CtxSelector::new(4);
        let mut rng = ChaCha8Rng::seed_from_u64(0);
        let mut hist = [0usize; 4];
        let n = 4096usize;
        for _ in 0..n {
            let h: u64 = rng.r#gen();
            hist[sel.pick(h) as usize] += 1;
        }
        let mean = n as f64 / 4.0;
        let lo = mean * 0.75;
        let hi = mean * 1.25;
        for (i, &c) in hist.iter().enumerate() {
            let cf = c as f64;
            assert!(
                cf >= lo && cf <= hi,
                "bucket {i} count {c} outside [{lo}, {hi}] (mean {mean})"
            );
        }
    }

    #[test]
    fn pick_with_one_context_is_zero() {
        let sel = CtxSelector::new(1);
        for h in [0u64, 1, 42, u64::MAX, 0xDEAD_BEEF_CAFE_F00D] {
            assert_eq!(sel.pick(h), 0);
        }
    }
}
