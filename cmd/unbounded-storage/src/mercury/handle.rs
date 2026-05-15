// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! RAII wrappers over Mercury's `hg_handle_t` and decoded output structs.
//!
//! The bare FFI threads handle lifetimes through every error path by hand:
//! `HG_Create` produces a handle, `HG_Forward`/`HG_Respond` consume it on
//! success, and the callback (or the caller) is responsible for `HG_Destroy`.
//! Likewise `HG_Get_output` allocates proc-owned fields that must be paired
//! with `HG_Free_output`. Both pairings become Rust ownership here so the
//! transport and server code no longer carry one `unsafe` block per call.

use std::ffi::c_void;
use std::ops::Deref;

use crate::mercury::class::ClassInner;
use crate::mercury::error::{HgError, Result, check};
use crate::mercury::ffi;

/// RAII wrapper around an `hg_handle_t`. Dropping the wrapper calls
/// `HG_Destroy`.
///
/// Ownership semantics intentionally mirror the FFI:
///
/// - `forward` consumes the wrapper because Mercury takes ownership of the
///   handle for the duration of the in-flight RPC; the forward callback
///   reconstructs a wrapper from the union variant and lets it drop.
/// - `respond` borrows the wrapper because the server-side flow awaits the
///   respond callback (which only signals completion) and then destroys the
///   handle on the executor thread by dropping the wrapper.
pub(crate) struct Handle(ffi::hg_handle_t);

impl Handle {
    /// Wrap an existing raw `hg_handle_t`.
    ///
    /// # Safety
    ///
    /// `raw` must be a live handle that the caller now exclusively owns.
    /// Typical sources: `Handle::into_raw`, the `forward.handle` field of
    /// `hg_cb_info` inside a forward callback, or the handle argument to a
    /// registered RPC callback.
    pub(crate) unsafe fn from_raw(raw: ffi::hg_handle_t) -> Self {
        Self(raw)
    }

    /// Borrow the raw handle without giving up ownership.
    pub(crate) fn as_raw(&self) -> ffi::hg_handle_t {
        self.0
    }

    /// Surrender ownership of the raw handle, suppressing `Drop`. The caller
    /// is responsible for destruction (directly via `HG_Destroy` or by
    /// reconstructing a `Handle` with `from_raw`).
    pub(crate) fn into_raw(self) -> ffi::hg_handle_t {
        let raw = self.0;
        std::mem::forget(self);
        raw
    }

    /// Create a client-side handle bound to `addr` on the class's context
    /// and registered RPC id.
    pub(crate) fn create(class: &ClassInner, addr: ffi::hg_addr_t) -> Result<Self> {
        let mut h: ffi::hg_handle_t = std::ptr::null_mut();
        // SAFETY: `hg_context` and `rpc_id` are class-owned and live for the
        // lifetime of `ClassInner`; `addr` is owned by the peer table.
        let ret = unsafe { ffi::HG_Create(class.hg_context, addr, class.rpc_id, &mut h) };
        check(ret, "HG_Create")?;
        Ok(Self(h))
    }

    /// Submit `HG_Forward`. Consumes `self`: on success the FFI owns the
    /// handle until its callback fires; on error the handle is destroyed
    /// by the wrapper's `Drop`.
    pub(crate) fn forward<I>(
        self,
        cb: ffi::hg_cb_t,
        arg: *mut c_void,
        input: &mut I,
    ) -> Result<()> {
        // SAFETY: handle is live; `input` is borrowed for the duration of
        // the call. Mercury copies any indirect bytes through the proc
        // synchronously, so `input` does not need to outlive this call.
        let ret = unsafe { ffi::HG_Forward(self.0, Some(cb), arg, input as *mut _ as *mut c_void) };
        if ret != ffi::HG_SUCCESS {
            // `self` drops here -> `HG_Destroy` runs.
            return Err(HgError::new(ret as i32, "HG_Forward"));
        }
        // Ownership has transferred to the FFI/callback.
        let _ = self.into_raw();
        Ok(())
    }

    /// Submit `HG_Respond` without giving up ownership of the handle.
    pub(crate) fn respond<O>(
        &self,
        cb: ffi::hg_cb_t,
        arg: *mut c_void,
        output: &mut O,
    ) -> Result<()> {
        // SAFETY: handle is live for the duration of `&self`; `output` is
        // borrowed for the call and Mercury copies through the proc.
        let ret =
            unsafe { ffi::HG_Respond(self.0, Some(cb), arg, output as *mut _ as *mut c_void) };
        check(ret, "HG_Respond")
    }

    /// Decode the incoming input struct via `HG_Get_input`. The caller is
    /// responsible for the matching `HG_Free_input` because Mercury's
    /// `HG_FREE` op also frees nested bulk handles, which the server
    /// pipeline still needs to drive after this call returns.
    pub(crate) fn get_input<I>(&self, out: &mut I) -> Result<()> {
        // SAFETY: handle is live; `out` is borrowed for the call. Mercury
        // decodes into `*out`.
        let ret = unsafe { ffi::HG_Get_input(self.0, out as *mut _ as *mut c_void) };
        check(ret, "HG_Get_input")
    }

    /// Free the proc-owned input previously decoded with `get_input`.
    pub(crate) fn free_input<I>(&self, out: &mut I) {
        // SAFETY: paired with a successful `get_input` on the same handle.
        unsafe {
            let _ = ffi::HG_Free_input(self.0, out as *mut _ as *mut c_void);
        }
    }

    /// Decode the output struct into a guard that calls `HG_Free_output`
    /// on drop.
    pub(crate) fn get_output<O>(&self, mut init: O) -> Result<OutputGuard<'_, O>> {
        // SAFETY: handle is live; `init` is owned by us until the guard
        // takes it.
        let ret = unsafe { ffi::HG_Get_output(self.0, &mut init as *mut _ as *mut c_void) };
        check(ret, "HG_Get_output")?;
        Ok(OutputGuard {
            handle: self,
            out: Some(init),
        })
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        // SAFETY: `self.0` was either produced by `HG_Create` or handed to
        // us by Mercury and is destroyed exactly once here.
        unsafe {
            let _ = ffi::HG_Destroy(self.0);
        }
    }
}

/// RAII guard returned by `Handle::get_output`. Derefs to the decoded
/// output struct; calls `HG_Free_output` on drop.
pub(crate) struct OutputGuard<'a, O> {
    handle: &'a Handle,
    out: Option<O>,
}

impl<O> Deref for OutputGuard<'_, O> {
    type Target = O;
    fn deref(&self) -> &O {
        self.out.as_ref().expect("output guard already consumed")
    }
}

impl<O> Drop for OutputGuard<'_, O> {
    fn drop(&mut self) {
        if let Some(mut o) = self.out.take() {
            // SAFETY: paired with the `HG_Get_output` call in
            // `Handle::get_output`; the handle is still live because we
            // hold a borrow of it.
            unsafe {
                let _ = ffi::HG_Free_output(self.handle.as_raw(), &mut o as *mut _ as *mut c_void);
            }
        }
    }
}
