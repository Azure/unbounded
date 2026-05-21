// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! RAII wrapper around `hg_bulk_t`.
//!
//! `LocalBulk` registers a single contiguous byte buffer with a Mercury
//! class via `HG_Bulk_create` and frees it via `HG_Bulk_free` on drop.

use std::os::raw::c_void;
use std::ptr::NonNull;

use super::error::{HgError, Result};
use super::ffi::{self, HG_SUCCESS, hg_bulk_s, hg_bulk_t, hg_class_t, hg_size_t, hg_uint8_t};

/// RAII wrapper around an `hg_bulk_t`.
///
/// Construction calls `HG_Bulk_create` over a single contiguous buffer
/// `[base, base + len)`. Drop calls `HG_Bulk_free`.
///
/// `LocalBulk` is `Send + Sync`: Mercury bulk handles are class-scoped
/// and may be referenced from any thread that can reach the owning
/// class. Mutating operations (transfer, free) take a mutable reference
/// to the underlying memory, but the handle itself is share-safe.
pub struct LocalBulk {
    raw: NonNull<hg_bulk_s>,
}

// SAFETY: Mercury's bulk handles are class-scoped and the underlying
// implementation is documented as thread-safe for read/use; we never
// expose interior mutability of the handle pointer itself.
unsafe impl Send for LocalBulk {}
// SAFETY: see Send impl above.
unsafe impl Sync for LocalBulk {}

impl LocalBulk {
    /// Register a single-buffer bulk handle over `[base, base + len)`
    /// against `class`. `flags` is one of `HG_BULK_READ_ONLY`,
    /// `HG_BULK_WRITE_ONLY`, `HG_BULK_READWRITE`.
    ///
    /// # Safety
    /// `base` must point to a valid, properly-aligned region of at
    /// least `len` bytes that remains live for the entire lifetime of
    /// the returned `LocalBulk`. Concurrent access to the underlying
    /// memory is the caller's responsibility - Mercury may DMA into or
    /// out of it asynchronously.
    pub unsafe fn over(
        class: hg_class_t,
        base: *mut u8,
        len: usize,
        flags: hg_uint8_t,
    ) -> Result<Self> {
        if class.is_null() {
            return Err(HgError::BadConfig("LocalBulk::over: null class"));
        }
        if base.is_null() {
            return Err(HgError::BadConfig("LocalBulk::over: null base"));
        }
        if len == 0 {
            return Err(HgError::BadConfig("LocalBulk::over: zero len"));
        }

        let mut bufs: [*mut c_void; 1] = [base as *mut c_void];
        let sizes: [hg_size_t; 1] = [len as hg_size_t];
        let mut handle: hg_bulk_t = std::ptr::null_mut();

        // SAFETY: bufs and sizes have one entry each; the buffer
        // lifetime is managed by the caller per the Safety contract
        // above. `&mut handle` points to a valid local out-parameter.
        let rc = unsafe {
            ffi::HG_Bulk_create(
                class,
                1,
                bufs.as_mut_ptr(),
                sizes.as_ptr(),
                flags,
                &mut handle,
            )
        };
        if rc != HG_SUCCESS {
            return Err(HgError::HgBulkCreate(rc));
        }
        let raw = NonNull::new(handle).ok_or(HgError::HgBulkCreate(rc))?;
        Ok(Self { raw })
    }

    /// The raw bulk handle. Pointer is valid for the lifetime of `&self`.
    pub fn as_raw(&self) -> hg_bulk_t {
        self.raw.as_ptr()
    }
}

impl Drop for LocalBulk {
    fn drop(&mut self) {
        // SAFETY: `self.raw` was non-null at construction and has not
        // been freed since (we are inside Drop). `HG_Bulk_free` accepts
        // a single owned handle.
        unsafe {
            let _ = ffi::HG_Bulk_free(self.raw.as_ptr());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mercury::ffi::HG_BULK_READWRITE;

    #[test]
    fn over_rejects_null_class() {
        let mut buf = [0u8; 64];
        // SAFETY: `over` returns before dereferencing any pointer
        // because the null-class check fires first.
        let r = unsafe {
            LocalBulk::over(
                std::ptr::null_mut(),
                buf.as_mut_ptr(),
                buf.len(),
                HG_BULK_READWRITE,
            )
        };
        assert!(matches!(r, Err(HgError::BadConfig(_))));
    }

    #[test]
    fn over_rejects_null_base() {
        let fake_class: hg_class_t = NonNull::dangling().as_ptr();
        // SAFETY: `over` returns before dereferencing any pointer
        // because the null-base check fires before any FFI call.
        let r = unsafe { LocalBulk::over(fake_class, std::ptr::null_mut(), 64, HG_BULK_READWRITE) };
        assert!(matches!(r, Err(HgError::BadConfig(_))));
    }

    #[test]
    fn over_rejects_zero_len() {
        let fake_class: hg_class_t = NonNull::dangling().as_ptr();
        let mut buf = [0u8; 64];
        // SAFETY: `over` returns before dereferencing any pointer
        // because the zero-len check fires before any FFI call.
        let r = unsafe { LocalBulk::over(fake_class, buf.as_mut_ptr(), 0, HG_BULK_READWRITE) };
        assert!(matches!(r, Err(HgError::BadConfig(_))));
    }
}
