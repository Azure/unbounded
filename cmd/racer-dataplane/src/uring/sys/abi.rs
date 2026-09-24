// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Linux queue, registration, probe, and enter layouts.
#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(crate) struct Sqe {
    pub opcode: u8,
    pub flags: u8,
    pub ioprio: u16,
    pub fd: i32,
    pub off: u64,
    pub addr: u64,
    pub len: u32,
    pub op_flags: u32,
    pub user_data: u64,
    pub buf_index: u16,
    pub personality: u16,
    pub file_index: u32,
    pub addr3: u64,
    pub pad: u64,
}
#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(crate) struct Cqe {
    pub user_data: u64,
    pub res: i32,
    pub flags: u32,
}
#[repr(C)]
#[derive(Default)]
pub(super) struct Offsets {
    pub head: u32,
    pub tail: u32,
    pub mask: u32,
    pub entries: u32,
    pub flags_or_overflow: u32,
    pub dropped_or_cqes: u32,
    pub array_or_flags: u32,
    pub reserved: u32,
    pub user_addr: u64,
}
#[repr(C)]
#[derive(Default)]
pub(super) struct Params {
    pub sq_entries: u32,
    pub cq_entries: u32,
    pub flags: u32,
    pub sq_thread_cpu: u32,
    pub sq_thread_idle: u32,
    pub features: u32,
    pub wq_fd: u32,
    pub reserved: [u32; 3],
    pub sq: Offsets,
    pub cq: Offsets,
}
#[repr(C)]
#[derive(Default)]
pub(super) struct Timespec {
    pub sec: i64,
    pub nsec: i64,
}
#[repr(C)]
#[derive(Default)]
pub(super) struct GetEvents {
    pub sigmask: u64,
    pub sigmask_sz: u32,
    pub pad: u32,
    pub ts: u64,
}
#[repr(C)]
#[derive(Default)]
pub(super) struct ProbeOp {
    pub op: u8,
    pub reserved: u8,
    pub flags: u16,
    pub reserved2: u32,
}
#[repr(C)]
pub(super) struct Probe {
    pub last_op: u8,
    pub len: u8,
    pub reserved: u16,
    pub reserved2: [u32; 3],
    pub ops: [ProbeOp; 64],
}
#[repr(C)]
pub(crate) struct FilesUpdate {
    pub offset: u32,
    pub reserved: u32,
    pub fds: u64,
}
pub(crate) const READ_FIXED: u8 = 4;
pub(crate) const WRITE_FIXED: u8 = 5;
pub(crate) const POLL: u8 = 6;
pub(crate) const ACCEPT: u8 = 13;
pub(crate) const CANCEL: u8 = 14;
pub(crate) const CONNECT: u8 = 16;
pub(crate) const READ: u8 = 22;
pub(crate) const SEND: u8 = 26;
pub(crate) const RECV: u8 = 27;
pub(crate) const SEND_ZC: u8 = 47;
pub(crate) const ASYNC: u8 = 1 << 4; // IOSQE_ASYNC
pub(crate) const MORE: u32 = 2;
pub(crate) const NOTIF: u32 = 8;
pub(crate) const WAKE: u64 = 0;
pub(crate) const CANCEL_ALL: u64 = u64::MAX;
pub(crate) const CANCEL_ABANDONED: u64 = 1;
const _: () = {
    assert!(size_of::<Sqe>() == 64);
    assert!(align_of::<Sqe>() == 8);
    assert!(std::mem::offset_of!(Sqe, off) == 8);
    assert!(std::mem::offset_of!(Sqe, addr) == 16);
    assert!(std::mem::offset_of!(Sqe, len) == 24);
    assert!(std::mem::offset_of!(Sqe, user_data) == 32);
    assert!(std::mem::offset_of!(Sqe, buf_index) == 40);
    assert!(std::mem::offset_of!(Sqe, addr3) == 48);
    assert!(size_of::<Cqe>() == 16);
    assert!(size_of::<Offsets>() == 40);
    assert!(size_of::<Params>() == 120);
    assert!(std::mem::offset_of!(Params, sq) == 40);
    assert!(std::mem::offset_of!(Params, cq) == 80);
    assert!(size_of::<GetEvents>() == 24);
    assert!(size_of::<Timespec>() == 16);
    assert!(std::mem::offset_of!(Probe, ops) == 16);
    assert!(size_of::<FilesUpdate>() == 16);
};
