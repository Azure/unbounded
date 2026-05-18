// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(non_camel_case_types)]
#![allow(dead_code)]

use std::os::raw::{c_char, c_int, c_uint, c_void};

pub type hg_return_t = c_int;
pub type hg_size_t = u64;
pub type hg_id_t = u64;
pub type hg_uint8_t = u8;
pub type hg_uint32_t = u32;
pub type hg_uint64_t = u64;
pub type hg_bool_t = u8;

#[repr(C)]
pub struct hg_class {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_context {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_handle {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_addr_s {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_bulk_s {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_op_id_s {
    _private: [u8; 0],
}
#[repr(C)]
pub struct hg_proc_s {
    _private: [u8; 0],
}

pub type hg_class_t = *mut hg_class;
pub type hg_context_t = *mut hg_context;
pub type hg_handle_t = *mut hg_handle;
pub type hg_addr_t = *mut hg_addr_s;
pub type hg_bulk_t = *mut hg_bulk_s;
pub type hg_op_id_t = *mut hg_op_id_s;
pub type hg_proc_t = *mut hg_proc_s;

pub const HG_SUCCESS: hg_return_t = 0;
pub const HG_TRUE: hg_bool_t = 1;
pub const HG_FALSE: hg_bool_t = 0;

// hg_cb_type_t values (mercury_core_types.h).
pub const HG_CB_LOOKUP: c_int = 0;
pub const HG_CB_FORWARD: c_int = 1;
pub const HG_CB_RESPOND: c_int = 2;
pub const HG_CB_BULK: c_int = 3;

// hg_bulk_op_t values (mercury_types.h).
pub const HG_BULK_PUSH: c_int = 0;
pub const HG_BULK_PULL: c_int = 1;

// Bulk permission flags (mercury_bulk.h).
pub const HG_BULK_READ_ONLY: hg_uint8_t = 1 << 0;
pub const HG_BULK_WRITE_ONLY: hg_uint8_t = 1 << 1;
pub const HG_BULK_READWRITE: hg_uint8_t = HG_BULK_READ_ONLY | HG_BULK_WRITE_ONLY;

// hg_cb_info layout - large union with a tagged variant. We project
// it as a fixed buffer because (1) we only care about a few fields
// and (2) layout-stable cross-version access via field offsets is
// risky. `hg_cb_info` is documented as having `ret`, `type`, `arg`
// at the end; `info` is the union at the front. We inspect what we
// need by knowing which `cb_type` we registered for the call.
#[repr(C)]
#[derive(Copy, Clone)]
pub struct hg_cb_info_forward {
    pub handle: hg_handle_t,
}

#[repr(C)]
#[derive(Copy, Clone)]
pub struct hg_cb_info_respond {
    pub handle: hg_handle_t,
}

#[repr(C)]
#[derive(Copy, Clone)]
pub struct hg_cb_info_lookup {
    pub addr: hg_addr_t,
}

#[repr(C)]
#[derive(Copy, Clone)]
pub struct hg_cb_info_bulk {
    pub origin_handle: hg_bulk_t,
    pub local_handle: hg_bulk_t,
    pub op: c_int,
    pub size: hg_size_t,
}

#[repr(C)]
pub union hg_cb_info_payload {
    pub lookup: hg_cb_info_lookup,
    pub forward: hg_cb_info_forward,
    pub respond: hg_cb_info_respond,
    pub bulk: hg_cb_info_bulk,
}

#[repr(C)]
pub struct hg_cb_info {
    pub info: hg_cb_info_payload,
    pub arg: *mut c_void,
    pub ty: c_int,
    pub ret: hg_return_t,
}

pub type hg_cb_t = unsafe extern "C" fn(callback_info: *const hg_cb_info) -> hg_return_t;
pub type hg_rpc_cb_t = unsafe extern "C" fn(handle: hg_handle_t) -> hg_return_t;
pub type hg_proc_cb_t = unsafe extern "C" fn(proc: hg_proc_t, data: *mut c_void) -> hg_return_t;

unsafe extern "C" {
    pub fn HG_Init_opt2(
        na_info_string: *const c_char,
        na_listen: hg_bool_t,
        version: c_uint,
        hg_init_info: *const c_void,
    ) -> hg_class_t;
    pub fn HG_Init(na_info_string: *const c_char, na_listen: hg_bool_t) -> hg_class_t;
    pub fn HG_Finalize(hg_class: hg_class_t) -> hg_return_t;

    pub fn HG_Context_create(hg_class: hg_class_t) -> hg_context_t;
    pub fn HG_Context_destroy(context: hg_context_t) -> hg_return_t;

    pub fn HG_Register_name(
        hg_class: hg_class_t,
        func_name: *const c_char,
        in_proc_cb: Option<hg_proc_cb_t>,
        out_proc_cb: Option<hg_proc_cb_t>,
        rpc_cb: Option<hg_rpc_cb_t>,
    ) -> hg_id_t;
    pub fn HG_Register_data(
        hg_class: hg_class_t,
        id: hg_id_t,
        data: *mut c_void,
        free_callback: Option<unsafe extern "C" fn(*mut c_void)>,
    ) -> hg_return_t;
    pub fn HG_Registered_data(hg_class: hg_class_t, id: hg_id_t) -> *mut c_void;

    pub fn HG_Addr_lookup2(
        hg_class: hg_class_t,
        name: *const c_char,
        addr_p: *mut hg_addr_t,
    ) -> hg_return_t;
    pub fn HG_Addr_self(hg_class: hg_class_t, addr_p: *mut hg_addr_t) -> hg_return_t;
    pub fn HG_Addr_free(hg_class: hg_class_t, addr: hg_addr_t) -> hg_return_t;
    pub fn HG_Addr_to_string(
        hg_class: hg_class_t,
        buf: *mut c_char,
        buf_size_p: *mut hg_size_t,
        addr: hg_addr_t,
    ) -> hg_return_t;

    pub fn HG_Create(
        context: hg_context_t,
        addr: hg_addr_t,
        id: hg_id_t,
        handle_p: *mut hg_handle_t,
    ) -> hg_return_t;
    pub fn HG_Destroy(handle: hg_handle_t) -> hg_return_t;
    pub fn HG_Forward(
        handle: hg_handle_t,
        callback: Option<hg_cb_t>,
        arg: *mut c_void,
        in_struct: *mut c_void,
    ) -> hg_return_t;
    pub fn HG_Respond(
        handle: hg_handle_t,
        callback: Option<hg_cb_t>,
        arg: *mut c_void,
        out_struct: *mut c_void,
    ) -> hg_return_t;
    pub fn HG_Cancel(handle: hg_handle_t) -> hg_return_t;
    pub fn HG_Get_input(handle: hg_handle_t, in_struct: *mut c_void) -> hg_return_t;
    pub fn HG_Free_input(handle: hg_handle_t, in_struct: *mut c_void) -> hg_return_t;
    pub fn HG_Get_output(handle: hg_handle_t, out_struct: *mut c_void) -> hg_return_t;
    pub fn HG_Free_output(handle: hg_handle_t, out_struct: *mut c_void) -> hg_return_t;

    pub fn HG_Progress(context: hg_context_t, timeout: c_uint) -> hg_return_t;
    pub fn HG_Trigger(
        context: hg_context_t,
        timeout: c_uint,
        max_count: c_uint,
        actual_count_p: *mut c_uint,
    ) -> hg_return_t;

    pub fn HG_Bulk_create(
        hg_class: hg_class_t,
        count: hg_uint32_t,
        buf_ptrs: *mut *mut c_void,
        buf_sizes: *const hg_size_t,
        flags: hg_uint8_t,
        handle: *mut hg_bulk_t,
    ) -> hg_return_t;
    pub fn HG_Bulk_free(handle: hg_bulk_t) -> hg_return_t;
    pub fn HG_Bulk_transfer(
        context: hg_context_t,
        callback: Option<hg_cb_t>,
        arg: *mut c_void,
        op: c_int,
        origin_addr: hg_addr_t,
        origin_handle: hg_bulk_t,
        origin_offset: hg_size_t,
        local_handle: hg_bulk_t,
        local_offset: hg_size_t,
        size: hg_size_t,
        op_id: *mut hg_op_id_t,
    ) -> hg_return_t;
}

// Mercury's `struct hg_info` carries the class, context, and addr for
// a handle; we only need the first three fields. Their layout is
// stable and documented in `mercury.h`.
#[repr(C)]
pub struct HgInfo {
    pub hg_class: hg_class_t,
    pub context: hg_context_t,
    pub addr: hg_addr_t,
    pub id: hg_id_t,
    pub context_id: hg_uint8_t,
}

// Shim symbols compiled from `shim.c`.
unsafe extern "C" {
    pub fn ub_proc_bulk_get_in(proc: hg_proc_t, data: *mut c_void) -> hg_return_t;
    pub fn ub_proc_bulk_get_out(proc: hg_proc_t, data: *mut c_void) -> hg_return_t;
    pub fn ub_sizeof_bulk_get_in() -> usize;
    pub fn ub_sizeof_bulk_get_out() -> usize;
    pub fn ub_handle_info(handle: hg_handle_t) -> *const HgInfo;
}

// ---------------------------------------------------------------------------
// Callback dispatch helpers.
//
// Every Mercury completion callback shares the same boilerplate: null-check
// `info`, deref it, and project out the fields the handler actually needs.
// The forward variant also has to read the `forward.handle` member of the
// tagged union. Centralizing the unsafe deref here means individual
// callbacks in `transport.rs` and `server.rs` no longer touch raw pointers
// or union fields.
// ---------------------------------------------------------------------------

/// Fields surfaced from a forward-RPC completion `hg_cb_info`.
pub struct ForwardCbInfo {
    pub handle: hg_handle_t,
    pub ret: hg_return_t,
    pub arg: *mut c_void,
}

/// Dispatch a forward-RPC completion callback. Null-checks `info`,
/// projects out the `forward` variant of the union, and hands it to `f`.
///
/// Safe to call from any `unsafe extern "C" fn` that Mercury invokes as a
/// forward callback. Mercury guarantees `info`, if non-null, points to a
/// valid `hg_cb_info` for the duration of the call.
pub fn dispatch_forward_cb(
    info: *const hg_cb_info,
    f: impl FnOnce(ForwardCbInfo) -> hg_return_t,
) -> hg_return_t {
    if info.is_null() {
        return HG_SUCCESS;
    }
    // SAFETY: per Mercury's callback contract, `info` is valid for the
    // duration of the callback. We register only forward callbacks against
    // forward RPCs, so the `forward` union variant is the live one.
    let cb = unsafe {
        let r = &*info;
        ForwardCbInfo {
            handle: r.info.forward.handle,
            ret: r.ret,
            arg: r.arg,
        }
    };
    f(cb)
}

/// Dispatch a callback that only needs `ret` and `arg` (bulk transfers
/// and respond completions). The union variant is not inspected.
pub fn dispatch_simple_cb(
    info: *const hg_cb_info,
    f: impl FnOnce(hg_return_t, *mut c_void) -> hg_return_t,
) -> hg_return_t {
    if info.is_null() {
        return HG_SUCCESS;
    }
    // SAFETY: same contract as `dispatch_forward_cb`.
    let (ret, arg) = unsafe {
        let r = &*info;
        (r.ret, r.arg)
    };
    f(ret, arg)
}
