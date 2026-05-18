// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use crate::mercury::error::{Result, check};
use crate::mercury::ffi;

/// Local bulk handle covering a single contiguous registered region.
pub(crate) struct LocalBulk {
    handle: ffi::hg_bulk_t,
    base: *mut u8,
    size: usize,
}

impl LocalBulk {
    pub(crate) fn register(
        hg_class: ffi::hg_class_t,
        base: *mut u8,
        size: usize,
        flags: ffi::hg_uint8_t,
    ) -> Result<Self> {
        let mut bufs: [*mut std::ffi::c_void; 1] = [base.cast()];
        let sizes: [ffi::hg_size_t; 1] = [size as ffi::hg_size_t];
        let mut handle: ffi::hg_bulk_t = std::ptr::null_mut();

        // SAFETY: `hg_class` is live; `bufs` and `sizes` are valid
        // for the duration of the call; `handle` is an out-pointer.
        let ret = unsafe {
            ffi::HG_Bulk_create(
                hg_class,
                1,
                bufs.as_mut_ptr(),
                sizes.as_ptr(),
                flags,
                &mut handle,
            )
        };
        check(ret, "HG_Bulk_create")?;
        Ok(Self { handle, base, size })
    }

    pub(crate) fn handle(&self) -> ffi::hg_bulk_t {
        self.handle
    }

    pub(crate) fn covers(&self, base: *mut u8, size: usize) -> bool {
        std::ptr::eq(self.base, base) && self.size == size
    }
}

impl Drop for LocalBulk {
    fn drop(&mut self) {
        if !self.handle.is_null() {
            // SAFETY: handle came from HG_Bulk_create and is freed
            // exactly once here.
            unsafe {
                ffi::HG_Bulk_free(self.handle);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a `LocalBulk` without touching Mercury so `covers` can
    /// be exercised in isolation. The null `handle` skips
    /// `HG_Bulk_free` in `Drop`.
    fn synthetic(base: *mut u8, size: usize) -> LocalBulk {
        LocalBulk {
            handle: std::ptr::null_mut(),
            base,
            size,
        }
    }

    #[test]
    fn covers_exact_match_is_true() {
        let mut buf = [0u8; 16];
        let b = synthetic(buf.as_mut_ptr(), buf.len());
        assert!(b.covers(buf.as_mut_ptr(), buf.len()));
    }

    #[test]
    fn covers_rejects_different_size() {
        let mut buf = [0u8; 16];
        let b = synthetic(buf.as_mut_ptr(), buf.len());
        assert!(!b.covers(buf.as_mut_ptr(), buf.len() - 1));
        assert!(!b.covers(buf.as_mut_ptr(), buf.len() + 1));
    }

    #[test]
    fn covers_rejects_different_base() {
        let mut a = [0u8; 16];
        let mut b = [0u8; 16];
        let bulk = synthetic(a.as_mut_ptr(), a.len());
        assert!(!bulk.covers(b.as_mut_ptr(), b.len()));
    }
}
