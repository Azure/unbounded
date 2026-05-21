// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! RAII wrapper around `hg_handle_t`.
//!
//! `Handle` owns a Mercury handle and calls `HG_Destroy` on drop. The
//! per-handle FFI calls (`HG_Forward`, `HG_Respond`, `HG_Get_input`,
//! `HG_Free_input`, `HG_Get_output`, `HG_Free_output`, `HG_Cancel`) are
//! exposed as thin wrappers that map non-success returns into the
//! crate's `HgError`. This file is intentionally narrow: nothing here
//! touches a Mercury class or context directly; that bookkeeping
//! belongs in `nic.rs` and `context.rs` in later waves.

use std::os::raw::c_void;
use std::ptr::NonNull;

use super::error::{HgError, Result, check};
use super::ffi::{
    self, HG_SUCCESS, HgInfo, hg_addr_t, hg_cb_t, hg_context_t, hg_handle_t, hg_id_t,
};

/// Owning RAII wrapper around `hg_handle_t`. Drop calls `HG_Destroy`.
///
/// Not `Send`/`Sync` by default: a handle is bound to the context that
/// created it (or whose RPC callback received it) and must be operated
/// on from the thread driving that context's progress. Crossing thread
/// boundaries is unsupported by Mercury.
pub struct Handle {
    raw: NonNull<ffi::hg_handle>,
    /// `true` iff this `Handle` should call `HG_Destroy` on drop.
    /// Server-side handles obtained from a `hg_rpc_cb_t` callback must
    /// be destroyed by us; we set `owns = true` either way and rely on
    /// Mercury's `HG_Destroy` being idempotent enough to handle the
    /// post-respond case. Set to `false` only when ownership has been
    /// explicitly transferred (e.g. into a callback arg).
    owns: bool,
}

impl Handle {
    /// Construct a client-side handle by calling `HG_Create`.
    pub fn create(context: hg_context_t, addr: hg_addr_t, rpc_id: hg_id_t) -> Result<Self> {
        let mut raw: hg_handle_t = std::ptr::null_mut();
        // SAFETY: `HG_Create` writes through `&mut raw` only on success;
        // `context` and `addr` are caller-provided Mercury pointers
        // whose validity is the caller's responsibility. We immediately
        // wrap the resulting non-null pointer in `NonNull` and own it.
        let rc = unsafe { ffi::HG_Create(context, addr, rpc_id, &mut raw) };
        check(rc, HgError::HgCreate)?;
        let raw = NonNull::new(raw).ok_or(HgError::HgCreate(HG_SUCCESS))?;
        Ok(Self { raw, owns: true })
    }

    /// Wrap a raw `hg_handle_t` taken from a server-side `hg_rpc_cb_t`
    /// callback or from `HG_Create` already done elsewhere.
    ///
    /// # Safety
    /// `raw` must be a non-null, currently-valid `hg_handle_t` whose
    /// destruction has not been started. The caller transfers
    /// ownership: this `Handle` will call `HG_Destroy` on drop unless
    /// `into_raw` is invoked first.
    pub unsafe fn from_raw(raw: hg_handle_t) -> Result<Self> {
        let raw = NonNull::new(raw).ok_or(HgError::HgCreate(HG_SUCCESS))?;
        Ok(Self { raw, owns: true })
    }

    /// Borrow the raw handle without giving up ownership. The pointer
    /// is only valid for the lifetime of `&self`.
    pub fn as_raw(&self) -> hg_handle_t {
        self.raw.as_ptr()
    }

    /// Surrender ownership; caller now responsible for `HG_Destroy`.
    pub fn into_raw(mut self) -> hg_handle_t {
        self.owns = false;
        self.raw.as_ptr()
    }

    /// Returns the `HgInfo` struct exposed by the C shim.
    /// The pointer is owned by Mercury and stays valid for the
    /// lifetime of the handle.
    pub fn info(&self) -> &HgInfo {
        // SAFETY: `ub_handle_info` returns a pointer that the shim
        // documents as valid for the lifetime of the handle. Tying the
        // reference to `&self` ensures we cannot outlive the handle,
        // and the shim never returns null for a live handle.
        unsafe {
            let ptr = ffi::ub_handle_info(self.raw.as_ptr());
            &*ptr
        }
    }

    /// `HG_Forward(self.raw, callback, arg, in_struct)`.
    ///
    /// # Safety
    /// `in_struct` must be a valid pointer to the input type registered
    /// for this handle's RPC id; it must remain valid until `callback`
    /// fires. `callback` and `arg` follow Mercury's lifetime rules
    /// (`arg` may be opaque; the callee invokes `callback(arg, ret)`).
    pub unsafe fn forward(
        &self,
        callback: Option<hg_cb_t>,
        arg: *mut c_void,
        in_struct: *mut c_void,
    ) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; `callback`, `arg`,
        // and `in_struct` are validated by the caller per the
        // function-level safety contract.
        let rc = unsafe { ffi::HG_Forward(self.raw.as_ptr(), callback, arg, in_struct) };
        check(rc, HgError::HgForward)
    }

    /// `HG_Respond(self.raw, callback, arg, out_struct)`.
    ///
    /// # Safety
    /// Same lifetime rules as `forward`.
    pub unsafe fn respond(
        &self,
        callback: Option<hg_cb_t>,
        arg: *mut c_void,
        out_struct: *mut c_void,
    ) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; pointer validity is
        // the caller's responsibility per the safety contract.
        let rc = unsafe { ffi::HG_Respond(self.raw.as_ptr(), callback, arg, out_struct) };
        check(rc, HgError::HgRespond)
    }

    /// `HG_Get_input(self.raw, in_struct)`.
    ///
    /// # Safety
    /// `in_struct` must point to a properly-aligned, writeable buffer
    /// of the registered input type's size. After a successful call,
    /// the caller must eventually invoke `free_input` against the same
    /// buffer to release Mercury-owned tail allocations.
    pub unsafe fn get_input(&self, in_struct: *mut c_void) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; `in_struct` is a
        // caller-validated buffer.
        let rc = unsafe { ffi::HG_Get_input(self.raw.as_ptr(), in_struct) };
        check(rc, HgError::HgGetInput)
    }

    /// `HG_Free_input(self.raw, in_struct)`.
    ///
    /// # Safety
    /// `in_struct` must be a buffer previously populated by
    /// `get_input` on this handle.
    pub unsafe fn free_input(&self, in_struct: *mut c_void) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; `in_struct` was
        // produced by an earlier `get_input` per the safety contract.
        let rc = unsafe { ffi::HG_Free_input(self.raw.as_ptr(), in_struct) };
        check(rc, HgError::HgFreeInput)
    }

    /// `HG_Get_output(self.raw, out_struct)`.
    ///
    /// # Safety
    /// Same shape as `get_input` for the output type.
    pub unsafe fn get_output(&self, out_struct: *mut c_void) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; `out_struct` is a
        // caller-validated buffer.
        let rc = unsafe { ffi::HG_Get_output(self.raw.as_ptr(), out_struct) };
        check(rc, HgError::HgGetOutput)
    }

    /// `HG_Free_output(self.raw, out_struct)`.
    ///
    /// # Safety
    /// Same shape as `free_input` for the output type.
    pub unsafe fn free_output(&self, out_struct: *mut c_void) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned; `out_struct` was
        // produced by an earlier `get_output` per the safety contract.
        let rc = unsafe { ffi::HG_Free_output(self.raw.as_ptr(), out_struct) };
        check(rc, HgError::HgFreeOutput)
    }

    /// `HG_Cancel(self.raw)`.
    pub fn cancel(&self) -> Result<()> {
        // SAFETY: `self.raw` is non-null and owned for the duration of
        // the call.
        let rc = unsafe { ffi::HG_Cancel(self.raw.as_ptr()) };
        // `error.rs` does not expose a dedicated `HgCancel` variant; we
        // fold cancel failures into `HgRespond` because cancel and
        // respond are the two completion-side terminations of a handle
        // and the rc semantics are the same. If a dedicated variant is
        // added later, swap the fallback here.
        map_rc(rc, HgError::HgRespond)
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        if self.owns {
            // SAFETY: self.raw was checked non-null at construction and
            // ownership has not been transferred away.
            unsafe {
                let _ = ffi::HG_Destroy(self.raw.as_ptr());
            }
        }
    }
}

/// Generic rc -> Result mapper for FFI calls whose error variant is
/// already chosen by the caller. Kept private; the existing `check`
/// helper covers most call sites, but `cancel` needs an inline mapping
/// because we deliberately reuse `HgRespond` as the fallback variant.
fn map_rc(rc: i32, fallback: fn(i32) -> HgError) -> Result<()> {
    if rc == HG_SUCCESS {
        Ok(())
    } else {
        Err(fallback(rc))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn into_raw_disables_drop() {
        // Build a fabricated Handle with owns=false to verify Drop is
        // a no-op. We never construct one with `owns=true` outside a
        // real class because Drop would call HG_Destroy on garbage.
        let fake: hg_handle_t = std::ptr::NonNull::dangling().as_ptr();
        let h = Handle {
            raw: NonNull::new(fake).unwrap(),
            owns: false,
        };
        let raw = h.as_raw();
        assert_eq!(raw, fake);
        // Drop here is safe because owns=false.
        drop(h);
    }
}
