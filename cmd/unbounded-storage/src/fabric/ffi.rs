// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// We keep `fi_info` and its sub-attribute structs opaque on the Rust
// side; the C shim in `shim.c` owns all field access so we never freeze
// a Rust layout against a moving target. The concrete structs below are
// the libfabric ABI types Rust initializes or reads field-wise; unit
// tests compare their sizes and offsets against the installed headers.

//! Hand-written libfabric FFI declarations plus shim externs for
//! libfabric's `static inline` entry points. Phase 3 keeps the
//! surface minimal: enough to bring up a fabric/domain/AV/EP/CQ
//! stack and progress completions, with everything reachable only
//! through pointer indirection so the inline-function gap stays
//! sealed behind `src/fabric/shim.c`.

#![allow(non_camel_case_types, dead_code)]

use std::os::raw::{c_char, c_int, c_void};

pub type fi_addr_t = u64;
pub type ssize_t = isize;

pub const FI_ADDR_UNSPEC: fi_addr_t = !0u64;

/// libfabric's `FI_EAGAIN`, returned negated by data-path calls
/// (`fi_tsend`, `fi_write`, ...) when the provider's outbound queue is
/// momentarily full or a connection-oriented provider (e.g. the `tcp`
/// RDM provider) has not yet finished its lazy connection handshake. It
/// is transient: the caller must retry after letting the CQ progress.
pub const FI_EAGAIN: i32 = 11;

/// System `ENOTCONN` as surfaced through a CQ error entry's
/// `prov_errno`. A connection-oriented provider (e.g. the `tcp` RDM
/// provider) reports this on a data-path operation (e.g. an `fi_write`
/// issued in the reverse direction of an just-received message) while
/// the lazy reverse connection is still being established. Like
/// `FI_EAGAIN` it is transient and the operation should be retried.
pub const ENOTCONN: i32 = 107;

pub const FI_VERSION: u32 = (1u32 << 16) | 20;

/// Version to request from `fi_getinfo`: the interface version this
/// crate is written against (`FI_VERSION`), capped to the version of
/// the libfabric actually linked at runtime. libfabric returns
/// `-FI_ENOSYS` when asked for a version newer than itself, so an
/// older installed library (e.g. 1.17) would otherwise fail outright.
pub fn requested_version() -> u32 {
    let runtime = unsafe { fi_version() };
    if runtime < FI_VERSION {
        runtime
    } else {
        FI_VERSION
    }
}

pub const FI_MSG: u64 = 1 << 1;
pub const FI_RMA: u64 = 1 << 2;
pub const FI_TAGGED: u64 = 1 << 3;
pub const FI_READ: u64 = 1 << 8;
pub const FI_WRITE: u64 = 1 << 9;
pub const FI_REMOTE_READ: u64 = 1 << 12;
pub const FI_REMOTE_WRITE: u64 = 1 << 13;
pub const FI_REMOTE_CQ_DATA: u64 = 1 << 17;
pub const FI_SOURCE: u64 = 1 << 57;

// fi_ep_type enum discriminants (rdma/fabric.h::enum fi_ep_type).
pub const FI_EP_UNSPEC: u32 = 0;
pub const FI_EP_MSG: u32 = 1;
pub const FI_EP_DGRAM: u32 = 2;
pub const FI_EP_RDM: u32 = 3;

// fi_progress enum discriminants (rdma/fabric.h::enum fi_progress).
pub const FI_PROGRESS_UNSPEC: u32 = 0;
pub const FI_PROGRESS_AUTO: u32 = 1;
pub const FI_PROGRESS_MANUAL: u32 = 2;

// fi_av_type enum discriminants.
pub const FI_AV_UNSPEC: u32 = 0;
pub const FI_AV_MAP: u32 = 1;
pub const FI_AV_TABLE: u32 = 2;

// fi_wait_obj enum discriminants.
pub const FI_WAIT_NONE: u32 = 0;
pub const FI_WAIT_UNSPEC: u32 = 1;

// fi_cq_format enum discriminants.
pub const FI_CQ_FORMAT_UNSPEC: u32 = 0;
pub const FI_CQ_FORMAT_CONTEXT: u32 = 1;
pub const FI_CQ_FORMAT_MSG: u32 = 2;
pub const FI_CQ_FORMAT_DATA: u32 = 3;
pub const FI_CQ_FORMAT_TAGGED: u32 = 4;

// MR mode bits (rdma/fabric.h `#define FI_MR_*`).
pub const FI_MR_LOCAL: i32 = 1 << 2;
pub const FI_MR_VIRT_ADDR: i32 = 1 << 4;
pub const FI_MR_ALLOCATED: i32 = 1 << 5;
pub const FI_MR_PROV_KEY: i32 = 1 << 6;

// fi_ep_bind flag bits we need. FI_TRANSMIT is an alias for FI_SEND
// and FI_RECV mirrors the capability bits in rdma/fabric.h.
pub const FI_TRANSMIT: u64 = 1 << 11;
pub const FI_RECV: u64 = 1 << 10;
pub const FI_SELECTIVE_COMPLETION: u64 = 1 << 59;

#[repr(C)]
pub struct fid {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_fabric {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_domain {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_ep {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_cq {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_av {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fid_mr {
    _private: [u8; 0],
}

/// Passive (listening) endpoint, used by the connection manager on the
/// server side. Opaque; only ever touched through shim wrappers.
#[repr(C)]
pub struct fid_pep {
    _private: [u8; 0],
}

/// Event queue. Carries connection-manager events (`FI_CONNREQ`,
/// `FI_CONNECTED`, `FI_SHUTDOWN`) for `FI_EP_MSG` endpoints.
#[repr(C)]
pub struct fid_eq {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_info {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_ep_attr {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_domain_attr {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_fabric_attr {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_tx_attr {
    _private: [u8; 0],
}

#[repr(C)]
pub struct fi_rx_attr {
    _private: [u8; 0],
}

/// CQ open attributes. The Rust side zero-initializes this and
/// writes `format` and `wait_obj`; everything else is left at
/// defaults. Layout matches libfabric 1.x (stable ABI).
#[repr(C)]
pub struct fi_cq_attr {
    pub size: usize,
    pub flags: u64,
    pub format: u32,
    pub wait_obj: u32,
    pub signaling_vector: c_int,
    pub wait_cond: u32,
    pub wait_set: *mut fid,
}

/// AV open attributes. Same approach: zero-init then write `type_`.
#[repr(C)]
pub struct fi_av_attr {
    pub type_: u32,
    pub rx_ctx_bits: c_int,
    pub count: usize,
    pub ep_per_node: usize,
    pub name: *const c_char,
    pub map_addr: *mut c_void,
    pub flags: u64,
}

/// One CQ data entry. Stable libfabric ABI; we read every field.
#[repr(C)]
pub struct fi_cq_data_entry {
    pub op_context: *mut c_void,
    pub flags: u64,
    pub len: usize,
    pub buf: *mut c_void,
    pub data: u64,
}

/// CQ tagged entry. Stable libfabric ABI; same prefix as
/// `fi_cq_data_entry` with `tag` appended. Used when the CQ is
/// opened with `FI_CQ_FORMAT_TAGGED`.
#[repr(C)]
pub struct fi_cq_tagged_entry {
    pub op_context: *mut c_void,
    pub flags: u64,
    pub len: usize,
    pub buf: *mut c_void,
    pub data: u64,
    pub tag: u64,
}

/// CQ error entry copied by the C shim from libfabric's native layout.
#[repr(C)]
pub struct fi_cq_err_entry {
    pub op_context: *mut c_void,
    pub flags: u64,
    pub len: usize,
    pub buf: *mut c_void,
    pub data: u64,
    pub tag: u64,
    pub olen: usize,
    pub err: c_int,
    pub prov_errno: c_int,
    pub err_data: *mut c_void,
    pub err_data_size: usize,
}

#[repr(C)]
pub struct fi_rma_iov {
    pub addr: u64,
    pub len: usize,
    pub key: u64,
}

/// Event-queue open attributes. Rust zero-initializes this and writes
/// `wait_obj`; everything else stays at provider defaults. Layout
/// matches libfabric 1.x (stable ABI).
#[repr(C)]
pub struct fi_eq_attr {
    pub size: usize,
    pub flags: u64,
    pub wait_obj: u32,
    pub signaling_vector: c_int,
    pub wait_set: *mut fid,
}

/// Connection-manager event payload read off the EQ for `FI_CONNREQ`,
/// `FI_CONNECTED`, and `FI_SHUTDOWN`. The trailing `data[]` flexible
/// array (connect private data) is not represented here; callers that
/// need it read past the struct. `info` is non-null only for
/// `FI_CONNREQ` and must be consumed (used to build the active EP) or
/// freed.
#[repr(C)]
pub struct fi_eq_cm_entry {
    pub fid: *mut fid,
    pub info: *mut fi_info,
}

/// EQ error entry, read via `ub_fi_eq_readerr` after the EQ reports
/// `-FI_EAVAIL`. Stable libfabric ABI.
#[repr(C)]
pub struct fi_eq_err_entry {
    pub fid: *mut fid,
    pub context: *mut c_void,
    pub data: u64,
    pub err: c_int,
    pub prov_errno: c_int,
    pub err_data: *mut c_void,
    pub err_data_size: usize,
}

unsafe extern "C" {
    // Exported libfabric symbols (not inline wrappers).
    pub fn fi_getinfo(
        version: u32,
        node: *const c_char,
        service: *const c_char,
        flags: u64,
        hints: *const fi_info,
        info: *mut *mut fi_info,
    ) -> c_int;

    pub fn fi_freeinfo(info: *mut fi_info);

    /// Runtime version of the linked libfabric, packed as
    /// `(major << 16) | minor`. Used to cap the version we request
    /// from `fi_getinfo`: asking for a newer version than the
    /// installed library returns `-FI_ENOSYS`.
    pub fn fi_version() -> u32;

    pub fn fi_fabric(
        attr: *mut fi_fabric_attr,
        fabric: *mut *mut fid_fabric,
        context: *mut c_void,
    ) -> c_int;

    // libfabric inline wrappers exported via src/fabric/shim.c.
    pub fn ub_fi_domain(
        fabric: *mut fid_fabric,
        info: *mut fi_info,
        domain: *mut *mut fid_domain,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_close(fid: *mut fid) -> c_int;
    pub fn ub_fi_endpoint(
        domain: *mut fid_domain,
        info: *mut fi_info,
        ep: *mut *mut fid_ep,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_dupinfo_with_dest(
        base: *mut fi_info,
        dest_addr: *const c_void,
        dest_addrlen: usize,
    ) -> *mut fi_info;
    pub fn ub_fi_enable(ep: *mut fid_ep) -> c_int;
    pub fn ub_fi_av_open(
        domain: *mut fid_domain,
        attr: *mut fi_av_attr,
        av: *mut *mut fid_av,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_cq_open(
        domain: *mut fid_domain,
        attr: *mut fi_cq_attr,
        cq: *mut *mut fid_cq,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_ep_bind(ep: *mut fid_ep, bfid: *mut fid, flags: u64) -> c_int;
    pub fn ub_fi_cq_read(cq: *mut fid_cq, buf: *mut c_void, count: usize) -> ssize_t;
    pub fn ub_fi_cq_readfrom(
        cq: *mut fid_cq,
        buf: *mut c_void,
        count: usize,
        src_addr: *mut fi_addr_t,
    ) -> ssize_t;
    pub fn ub_fi_cq_readerr(cq: *mut fid_cq, buf: *mut fi_cq_err_entry, flags: u64) -> ssize_t;
    pub fn ub_fi_getname(fid: *mut fid, addr: *mut c_void, addrlen: *mut usize) -> c_int;

    pub fn ub_fi_build_hints(prov_name: *const c_char) -> *mut fi_info;
    pub fn ub_fi_hints_set_domain(hints: *mut fi_info, name: *const c_char) -> c_int;
    pub fn ub_fi_hints_set_src_addr(
        hints: *mut fi_info,
        addr: *const c_void,
        addrlen: usize,
    ) -> c_int;
    pub fn ub_fi_info_fabric_attr(info: *mut fi_info) -> *mut fi_fabric_attr;
    pub fn ub_fi_info_mr_mode(info: *mut fi_info) -> c_int;
    pub fn ub_fi_info_threading(info: *mut fi_info) -> c_int;
    pub fn ub_fi_thread_safe_value() -> c_int;
    pub fn ub_fi_info_mr_cnt(info: *mut fi_info) -> usize;

    pub fn ub_fi_eagain() -> c_int;
    pub fn ub_fi_eavail() -> c_int;
    pub fn ub_fi_enodata() -> c_int;

    pub fn ub_fi_av_insert(
        av: *mut fid_av,
        addr: *const c_void,
        count: usize,
        fi_addr: *mut fi_addr_t,
        flags: u64,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_av_remove(
        av: *mut fid_av,
        fi_addr: *mut fi_addr_t,
        count: usize,
        flags: u64,
    ) -> c_int;

    pub fn ub_fi_mr_reg(
        domain: *mut fid_domain,
        buf: *const c_void,
        len: usize,
        access: u64,
        offset: u64,
        requested_key: u64,
        flags: u64,
        mr: *mut *mut fid_mr,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_mr_key(mr: *mut fid_mr) -> u64;
    pub fn ub_fi_mr_desc(mr: *mut fid_mr) -> *mut c_void;

    pub fn ub_fi_tsend(
        ep: *mut fid_ep,
        buf: *const c_void,
        len: usize,
        desc: *mut c_void,
        dest_addr: fi_addr_t,
        tag: u64,
        context: *mut c_void,
    ) -> ssize_t;
    pub fn ub_fi_trecv(
        ep: *mut fid_ep,
        buf: *mut c_void,
        len: usize,
        desc: *mut c_void,
        src_addr: fi_addr_t,
        tag: u64,
        ignore: u64,
        context: *mut c_void,
    ) -> ssize_t;
    pub fn ub_fi_send(
        ep: *mut fid_ep,
        buf: *const c_void,
        len: usize,
        desc: *mut c_void,
        dest_addr: fi_addr_t,
        context: *mut c_void,
    ) -> ssize_t;
    pub fn ub_fi_recv(
        ep: *mut fid_ep,
        buf: *mut c_void,
        len: usize,
        desc: *mut c_void,
        src_addr: fi_addr_t,
        context: *mut c_void,
    ) -> ssize_t;

    pub fn ub_fi_parse_sockaddr(s: *const c_char, out: *mut u8, out_cap: usize) -> ssize_t;
    pub fn ub_fi_format_sockaddr(
        addr: *const c_void,
        addrlen: usize,
        out: *mut c_char,
        cap: usize,
    ) -> ssize_t;

    pub fn ub_fi_write(
        ep: *mut fid_ep,
        buf: *const c_void,
        len: usize,
        desc: *mut c_void,
        dest_addr: fi_addr_t,
        addr: u64,
        key: u64,
        context: *mut c_void,
    ) -> ssize_t;

    pub fn ub_fi_writedata(
        ep: *mut fid_ep,
        buf: *const c_void,
        len: usize,
        desc: *mut c_void,
        data: u64,
        dest_addr: fi_addr_t,
        addr: u64,
        key: u64,
        context: *mut c_void,
    ) -> ssize_t;

    pub fn ub_fi_cancel(fid: *mut fid, context: *mut c_void) -> ssize_t;

    // Connection-manager wrappers (FI_EP_MSG). See src/fabric/shim.c.
    pub fn ub_fi_build_msg_hints(prov_name: *const c_char) -> *mut fi_info;
    pub fn ub_fi_eq_open(
        fabric: *mut fid_fabric,
        attr: *mut fi_eq_attr,
        eq: *mut *mut fid_eq,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_passive_ep(
        fabric: *mut fid_fabric,
        info: *mut fi_info,
        pep: *mut *mut fid_pep,
        context: *mut c_void,
    ) -> c_int;
    pub fn ub_fi_pep_bind(pep: *mut fid_pep, bfid: *mut fid, flags: u64) -> c_int;
    pub fn ub_fi_listen(pep: *mut fid_pep) -> c_int;
    pub fn ub_fi_ep_bind_eq(ep: *mut fid_ep, eq: *mut fid_eq, flags: u64) -> c_int;
    pub fn ub_fi_connect(
        ep: *mut fid_ep,
        addr: *const c_void,
        param: *const c_void,
        paramlen: usize,
    ) -> c_int;
    pub fn ub_fi_accept(ep: *mut fid_ep, param: *const c_void, paramlen: usize) -> c_int;
    pub fn ub_fi_eq_sread(
        eq: *mut fid_eq,
        event: *mut u32,
        buf: *mut c_void,
        len: usize,
        timeout: c_int,
        flags: u64,
    ) -> ssize_t;
    pub fn ub_fi_eq_read(
        eq: *mut fid_eq,
        event: *mut u32,
        buf: *mut c_void,
        len: usize,
        flags: u64,
    ) -> ssize_t;
    pub fn ub_fi_eq_readerr(eq: *mut fid_eq, buf: *mut fi_eq_err_entry, flags: u64) -> ssize_t;
    pub fn ub_fi_connreq() -> u32;
    pub fn ub_fi_connected() -> u32;
    pub fn ub_fi_shutdown() -> u32;

    #[cfg(test)]
    fn ub_fi_layout(type_: c_int, field: c_int) -> usize;
}

/// Cast helpers: every libfabric resource has its `struct fid` at
/// offset 0, so a pointer to the resource is also a valid `*mut fid`.
/// We funnel all casts through these inline helpers to make the
/// intent explicit at call sites.
#[inline]
pub fn as_fid_fabric(p: *mut fid_fabric) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_domain(p: *mut fid_domain) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_ep(p: *mut fid_ep) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_cq(p: *mut fid_cq) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_av(p: *mut fid_av) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_mr(p: *mut fid_mr) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_pep(p: *mut fid_pep) -> *mut fid {
    p as *mut fid
}
#[inline]
pub fn as_fid_eq(p: *mut fid_eq) -> *mut fid {
    p as *mut fid
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::mem::{offset_of, size_of};

    const FI_CQ_ATTR: c_int = 1;
    const FI_AV_ATTR: c_int = 2;
    const FI_CQ_DATA_ENTRY: c_int = 3;
    const FI_CQ_TAGGED_ENTRY: c_int = 4;
    const FI_CQ_ERR_ENTRY: c_int = 5;
    const FI_RMA_IOV: c_int = 6;
    const FI_EQ_ATTR: c_int = 7;
    const FI_EQ_CM_ENTRY: c_int = 8;
    const FI_EQ_ERR_ENTRY: c_int = 9;

    const SIZE: c_int = 0;
    const CQ_ATTR_SIZE: c_int = 1;
    const CQ_ATTR_FLAGS: c_int = 2;
    const CQ_ATTR_FORMAT: c_int = 3;
    const CQ_ATTR_WAIT_OBJ: c_int = 4;
    const CQ_ATTR_SIGNALING_VECTOR: c_int = 5;
    const CQ_ATTR_WAIT_COND: c_int = 6;
    const CQ_ATTR_WAIT_SET: c_int = 7;
    const AV_ATTR_TYPE: c_int = 8;
    const AV_ATTR_RX_CTX_BITS: c_int = 9;
    const AV_ATTR_COUNT: c_int = 10;
    const AV_ATTR_EP_PER_NODE: c_int = 11;
    const AV_ATTR_NAME: c_int = 12;
    const AV_ATTR_MAP_ADDR: c_int = 13;
    const AV_ATTR_FLAGS: c_int = 14;
    const CQ_ENTRY_OP_CONTEXT: c_int = 15;
    const CQ_ENTRY_FLAGS: c_int = 16;
    const CQ_ENTRY_LEN: c_int = 17;
    const CQ_ENTRY_BUF: c_int = 18;
    const CQ_ENTRY_DATA: c_int = 19;
    const CQ_ENTRY_TAG: c_int = 20;
    const CQ_ERR_ENTRY_OLEN: c_int = 21;
    const CQ_ERR_ENTRY_ERR: c_int = 22;
    const CQ_ERR_ENTRY_PROV_ERRNO: c_int = 23;
    const CQ_ERR_ENTRY_ERR_DATA: c_int = 24;
    const CQ_ERR_ENTRY_ERR_DATA_SIZE: c_int = 25;
    const RMA_IOV_ADDR: c_int = 27;
    const RMA_IOV_LEN: c_int = 28;
    const RMA_IOV_KEY: c_int = 29;
    const EQ_ATTR_SIZE: c_int = 30;
    const EQ_ATTR_FLAGS: c_int = 31;
    const EQ_ATTR_WAIT_OBJ: c_int = 32;
    const EQ_ATTR_SIGNALING_VECTOR: c_int = 33;
    const EQ_ATTR_WAIT_SET: c_int = 34;
    const EQ_CM_ENTRY_FID: c_int = 35;
    const EQ_CM_ENTRY_INFO: c_int = 36;
    const EQ_ERR_ENTRY_FID: c_int = 37;
    const EQ_ERR_ENTRY_CONTEXT: c_int = 38;
    const EQ_ERR_ENTRY_DATA: c_int = 39;
    const EQ_ERR_ENTRY_ERR: c_int = 40;
    const EQ_ERR_ENTRY_PROV_ERRNO: c_int = 41;
    const EQ_ERR_ENTRY_ERR_DATA: c_int = 42;
    const EQ_ERR_ENTRY_ERR_DATA_SIZE: c_int = 43;

    fn c_layout(type_: c_int, field: c_int) -> usize {
        unsafe { ub_fi_layout(type_, field) }
    }

    fn assert_size(type_: c_int, rust_size: usize) {
        assert_eq!(c_layout(type_, SIZE), rust_size);
    }

    fn assert_offset(type_: c_int, field: c_int, rust_offset: usize) {
        assert_eq!(c_layout(type_, field), rust_offset);
    }

    #[test]
    fn libfabric_struct_layouts_match_installed_headers() {
        assert_size(FI_CQ_ATTR, size_of::<fi_cq_attr>());
        assert_offset(FI_CQ_ATTR, CQ_ATTR_SIZE, offset_of!(fi_cq_attr, size));
        assert_offset(FI_CQ_ATTR, CQ_ATTR_FLAGS, offset_of!(fi_cq_attr, flags));
        assert_offset(FI_CQ_ATTR, CQ_ATTR_FORMAT, offset_of!(fi_cq_attr, format));
        assert_offset(
            FI_CQ_ATTR,
            CQ_ATTR_WAIT_OBJ,
            offset_of!(fi_cq_attr, wait_obj),
        );
        assert_offset(
            FI_CQ_ATTR,
            CQ_ATTR_SIGNALING_VECTOR,
            offset_of!(fi_cq_attr, signaling_vector),
        );
        assert_offset(
            FI_CQ_ATTR,
            CQ_ATTR_WAIT_COND,
            offset_of!(fi_cq_attr, wait_cond),
        );
        assert_offset(
            FI_CQ_ATTR,
            CQ_ATTR_WAIT_SET,
            offset_of!(fi_cq_attr, wait_set),
        );

        assert_size(FI_AV_ATTR, size_of::<fi_av_attr>());
        assert_offset(FI_AV_ATTR, AV_ATTR_TYPE, offset_of!(fi_av_attr, type_));
        assert_offset(
            FI_AV_ATTR,
            AV_ATTR_RX_CTX_BITS,
            offset_of!(fi_av_attr, rx_ctx_bits),
        );
        assert_offset(FI_AV_ATTR, AV_ATTR_COUNT, offset_of!(fi_av_attr, count));
        assert_offset(
            FI_AV_ATTR,
            AV_ATTR_EP_PER_NODE,
            offset_of!(fi_av_attr, ep_per_node),
        );
        assert_offset(FI_AV_ATTR, AV_ATTR_NAME, offset_of!(fi_av_attr, name));
        assert_offset(
            FI_AV_ATTR,
            AV_ATTR_MAP_ADDR,
            offset_of!(fi_av_attr, map_addr),
        );
        assert_offset(FI_AV_ATTR, AV_ATTR_FLAGS, offset_of!(fi_av_attr, flags));

        assert_size(FI_CQ_DATA_ENTRY, size_of::<fi_cq_data_entry>());
        assert_offset(
            FI_CQ_DATA_ENTRY,
            CQ_ENTRY_OP_CONTEXT,
            offset_of!(fi_cq_data_entry, op_context),
        );
        assert_offset(
            FI_CQ_DATA_ENTRY,
            CQ_ENTRY_FLAGS,
            offset_of!(fi_cq_data_entry, flags),
        );
        assert_offset(
            FI_CQ_DATA_ENTRY,
            CQ_ENTRY_LEN,
            offset_of!(fi_cq_data_entry, len),
        );
        assert_offset(
            FI_CQ_DATA_ENTRY,
            CQ_ENTRY_BUF,
            offset_of!(fi_cq_data_entry, buf),
        );
        assert_offset(
            FI_CQ_DATA_ENTRY,
            CQ_ENTRY_DATA,
            offset_of!(fi_cq_data_entry, data),
        );

        assert_size(FI_CQ_TAGGED_ENTRY, size_of::<fi_cq_tagged_entry>());
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_OP_CONTEXT,
            offset_of!(fi_cq_tagged_entry, op_context),
        );
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_FLAGS,
            offset_of!(fi_cq_tagged_entry, flags),
        );
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_LEN,
            offset_of!(fi_cq_tagged_entry, len),
        );
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_BUF,
            offset_of!(fi_cq_tagged_entry, buf),
        );
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_DATA,
            offset_of!(fi_cq_tagged_entry, data),
        );
        assert_offset(
            FI_CQ_TAGGED_ENTRY,
            CQ_ENTRY_TAG,
            offset_of!(fi_cq_tagged_entry, tag),
        );

        assert_size(FI_CQ_ERR_ENTRY, size_of::<fi_cq_err_entry>());
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_OP_CONTEXT,
            offset_of!(fi_cq_err_entry, op_context),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_FLAGS,
            offset_of!(fi_cq_err_entry, flags),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_LEN,
            offset_of!(fi_cq_err_entry, len),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_BUF,
            offset_of!(fi_cq_err_entry, buf),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_DATA,
            offset_of!(fi_cq_err_entry, data),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ENTRY_TAG,
            offset_of!(fi_cq_err_entry, tag),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ERR_ENTRY_OLEN,
            offset_of!(fi_cq_err_entry, olen),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ERR_ENTRY_ERR,
            offset_of!(fi_cq_err_entry, err),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ERR_ENTRY_PROV_ERRNO,
            offset_of!(fi_cq_err_entry, prov_errno),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ERR_ENTRY_ERR_DATA,
            offset_of!(fi_cq_err_entry, err_data),
        );
        assert_offset(
            FI_CQ_ERR_ENTRY,
            CQ_ERR_ENTRY_ERR_DATA_SIZE,
            offset_of!(fi_cq_err_entry, err_data_size),
        );
        assert_size(FI_RMA_IOV, size_of::<fi_rma_iov>());
        assert_offset(FI_RMA_IOV, RMA_IOV_ADDR, offset_of!(fi_rma_iov, addr));
        assert_offset(FI_RMA_IOV, RMA_IOV_LEN, offset_of!(fi_rma_iov, len));
        assert_offset(FI_RMA_IOV, RMA_IOV_KEY, offset_of!(fi_rma_iov, key));

        assert_size(FI_EQ_ATTR, size_of::<fi_eq_attr>());
        assert_offset(FI_EQ_ATTR, EQ_ATTR_SIZE, offset_of!(fi_eq_attr, size));
        assert_offset(FI_EQ_ATTR, EQ_ATTR_FLAGS, offset_of!(fi_eq_attr, flags));
        assert_offset(
            FI_EQ_ATTR,
            EQ_ATTR_WAIT_OBJ,
            offset_of!(fi_eq_attr, wait_obj),
        );
        assert_offset(
            FI_EQ_ATTR,
            EQ_ATTR_SIGNALING_VECTOR,
            offset_of!(fi_eq_attr, signaling_vector),
        );
        assert_offset(
            FI_EQ_ATTR,
            EQ_ATTR_WAIT_SET,
            offset_of!(fi_eq_attr, wait_set),
        );

        assert_size(FI_EQ_CM_ENTRY, size_of::<fi_eq_cm_entry>());
        assert_offset(
            FI_EQ_CM_ENTRY,
            EQ_CM_ENTRY_FID,
            offset_of!(fi_eq_cm_entry, fid),
        );
        assert_offset(
            FI_EQ_CM_ENTRY,
            EQ_CM_ENTRY_INFO,
            offset_of!(fi_eq_cm_entry, info),
        );

        assert_size(FI_EQ_ERR_ENTRY, size_of::<fi_eq_err_entry>());
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_FID,
            offset_of!(fi_eq_err_entry, fid),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_CONTEXT,
            offset_of!(fi_eq_err_entry, context),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_DATA,
            offset_of!(fi_eq_err_entry, data),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_ERR,
            offset_of!(fi_eq_err_entry, err),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_PROV_ERRNO,
            offset_of!(fi_eq_err_entry, prov_errno),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_ERR_DATA,
            offset_of!(fi_eq_err_entry, err_data),
        );
        assert_offset(
            FI_EQ_ERR_ENTRY,
            EQ_ERR_ENTRY_ERR_DATA_SIZE,
            offset_of!(fi_eq_err_entry, err_data_size),
        );
    }
}
