//! ublk UAPI and runtime-specific device handling.
//!
//! Hand-written bindings for `include/uapi/linux/ublk_cmd.h`. Control commands are
//! `uring_cmd`s carrying a 32-byte `ublksrv_ctrl_cmd` in 128-byte SQEs, each synchronous.

use super::sys;

mod control;
mod worker;

pub(super) use control::add_dev;
pub(super) use control::{Control, assign_queues};
pub(super) use worker::{Ctl as WorkerCtl, State as WorkerState};

#[cfg(test)]
pub(super) fn test_tag_id(slot: u16, queue: usize, tag: u16) -> u32 {
    worker::tag_id(slot, queue, tag)
}

#[cfg(test)]
pub(super) fn test_tag_parts(id: u32) -> (u16, usize, u16) {
    worker::tag_parts(id)
}

pub(super) fn start_queue(
    slot: u16,
    dev: u64,
    cfd: crate::kernel::FileRef,
    depth: u16,
    q_ids: Vec<u16>,
    ack: super::worker::Ack,
) -> WorkerCtl {
    WorkerCtl::Start {
        slot,
        dev,
        cfd,
        depth,
        q_ids,
        ack,
    }
}

pub(super) fn stop_queue(slot: u16, ack: super::worker::Ack) -> WorkerCtl {
    WorkerCtl::Stop { slot, ack }
}

pub(super) fn reap_queue(slot: u16, ack: super::worker::Ack) -> WorkerCtl {
    WorkerCtl::Reap { slot, ack }
}

pub(super) fn request_target(id: u32) -> Result<(u32, u16, u16), super::Errno> {
    worker::request_target(id)
}

// --- ioctl encoding ---

const IOC_NRBITS: u32 = 8;
const IOC_TYPEBITS: u32 = 8;
const IOC_SIZEBITS: u32 = 14;
const IOC_NRSHIFT: u32 = 0;
const IOC_TYPESHIFT: u32 = IOC_NRSHIFT + IOC_NRBITS;
const IOC_SIZESHIFT: u32 = IOC_TYPESHIFT + IOC_TYPEBITS;
const IOC_DIRSHIFT: u32 = IOC_SIZESHIFT + IOC_SIZEBITS;
const IOC_WRITE: u32 = 1;
const IOC_READ: u32 = 2;

const fn ioc(dir: u32, ty: u32, nr: u32, size: u32) -> u32 {
    (dir << IOC_DIRSHIFT) | (ty << IOC_TYPESHIFT) | (nr << IOC_NRSHIFT) | (size << IOC_SIZESHIFT)
}
const fn ior(nr: u32, size: u32) -> u32 {
    ioc(IOC_READ, b'u' as u32, nr, size)
}
const fn iowr(nr: u32, size: u32) -> u32 {
    ioc(IOC_READ | IOC_WRITE, b'u' as u32, nr, size)
}

const CTRL_SZ: u32 = size_of::<CtrlCmd>() as u32;
const IO_SZ: u32 = size_of::<IoCmd>() as u32;

const CMD_GET_QUEUE_AFFINITY: u32 = ior(0x01, CTRL_SZ);
const CMD_GET_DEV_INFO: u32 = ior(0x02, CTRL_SZ);
const CMD_ADD_DEV: u32 = iowr(0x04, CTRL_SZ);
const CMD_DEL_DEV: u32 = iowr(0x05, CTRL_SZ);
const CMD_START_DEV: u32 = iowr(0x06, CTRL_SZ);
const CMD_STOP_DEV: u32 = iowr(0x07, CTRL_SZ);
const CMD_SET_PARAMS: u32 = iowr(0x08, CTRL_SZ);
const CMD_GET_FEATURES: u32 = ior(0x13, CTRL_SZ);
const CMD_DEL_DEV_ASYNC: u32 = ior(0x14, CTRL_SZ);

/// The control commands, for the test that pins them against the simulated driver's own
/// copy. The two are written out independently on purpose: they are the two sides of one
/// wire, and a wire nobody can get wrong is a wire nobody is testing.
#[cfg(test)]
pub(super) const TEST_IO_CMDS: [u32; 4] = [
    IO_FETCH_REQ,
    IO_COMMIT_AND_FETCH_REQ,
    IO_REGISTER_IO_BUF,
    IO_UNREGISTER_IO_BUF,
];

#[cfg(test)]
pub(super) const TEST_CTRL_CMDS: [u32; 9] = [
    CMD_GET_QUEUE_AFFINITY,
    CMD_GET_DEV_INFO,
    CMD_ADD_DEV,
    CMD_DEL_DEV,
    CMD_START_DEV,
    CMD_STOP_DEV,
    CMD_SET_PARAMS,
    CMD_GET_FEATURES,
    CMD_DEL_DEV_ASYNC,
];

pub(super) const IO_FETCH_REQ: u32 = iowr(0x20, IO_SZ);
pub(super) const IO_COMMIT_AND_FETCH_REQ: u32 = iowr(0x21, IO_SZ);
pub(super) const IO_REGISTER_IO_BUF: u32 = iowr(0x23, IO_SZ);
pub(super) const IO_UNREGISTER_IO_BUF: u32 = iowr(0x24, IO_SZ);

// --- feature flags, states, ops ---

/// `UBLK_F_CMD_IOCTL_ENCODE`: commands are the ioctl encodings above, not bare numbers.
pub(super) const F_CMD_IOCTL_ENCODE: u64 = 1 << 6;
/// `UBLK_F_USER_COPY`: the payload is also reachable by `pread`/`pwrite` on `/dev/ublkcN`
/// at [`buf_offset`]. Not exclusive with `F_AUTO_BUF_REG`: `ublk_need_map_io()` stays
/// false and `ublk_check_and_get_req()` gates on this bit alone. We set both and pick per
/// request: a copy for client pages the allocator checksums, zero copy for fabric frames.
pub(super) const F_USER_COPY: u64 = 1 << 7;
/// `UBLK_F_AUTO_BUF_REG`: the kernel registers the request buffer into our ring as part of
/// FETCH. Unlike `UBLK_F_SUPPORT_ZERO_COPY` (1 << 0), it costs no extra uring_cmds per IO.
pub(super) const F_AUTO_BUF_REG: u64 = 1 << 11;

/// What the node refuses to run without, for the test that pins it against the simulated
/// driver's offering.
#[cfg(test)]
pub(super) const TEST_REQUIRED_FEATURES: u64 = F_CMD_IOCTL_ENCODE | F_USER_COPY | F_AUTO_BUF_REG;

pub(super) const IO_RES_OK: i32 = 0;

pub(super) const OP_READ: u8 = 0;
pub(super) const OP_WRITE: u8 = 1;
pub(super) const OP_DISCARD: u8 = 3;

/// Set when auto buffer registration fell back and we must register by hand.
pub(super) const IO_F_NEED_REG_BUF: u32 = 1 << 17;

pub(super) const AUTO_BUF_REG_FALLBACK: u8 = 1 << 0;

const MAX_QUEUE_DEPTH: u32 = 4096;

/// `max_io_buf_bytes`: the largest single request the block layer may send us, and so the
/// largest buffer a handler ever sees. 8 KiB, one fabric frame: a 4 KiB block and its
/// 4 KiB trailer. A client device never sends more than one 4 KiB block.
pub(crate) const MAX_IO_BYTES: usize = 8 << 10;

/// `UBLKSRV_IO_BUF_OFFSET`, with `(q_id, tag)` packed into the bits above it.
const IO_BUF_OFFSET: u64 = 0x8000_0000;
const TAG_OFF: u32 = 25;
const QID_OFF: u32 = 41;

/// File offset of a request's payload within `/dev/ublkcN`, for `USER_COPY`.
pub(crate) fn buf_offset(q_id: u16, tag: u16, off: usize) -> u64 {
    IO_BUF_OFFSET + ((q_id as u64) << QID_OFF) + ((tag as u64) << TAG_OFF) + off as u64
}

/// Kernel's per-queue io_desc buffer stride; see `ublk_max_cmd_buf_size()`.
pub(crate) fn cmd_buf_size() -> usize {
    (MAX_QUEUE_DEPTH as usize * size_of::<IoDesc>()).next_multiple_of(sys::page_size())
}

// --- structs ---

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(crate) struct CtrlCmd {
    pub(crate) dev_id: u32,
    pub(crate) queue_id: u16,
    pub(crate) len: u16,
    pub(crate) addr: u64,
    pub(crate) data: [u64; 1],
    pub(crate) dev_path_len: u16,
    pub(crate) pad: u16,
    pub(crate) reserved: u32,
}

impl DevInfo {
    /// A device request at the minor `dev_id`. The node asks for the number rather than
    /// taking what the kernel hands out: the path is `/dev/ublkb<dev_id>`, and the control
    /// plane published it before this process started.
    pub(crate) fn new(dev_id: u32, nr_hw_queues: u16, queue_depth: u16, flags: u64) -> DevInfo {
        DevInfo {
            nr_hw_queues,
            queue_depth,
            dev_id,
            max_io_buf_bytes: MAX_IO_BYTES as u32,
            flags,
            ..Default::default()
        }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub(crate) struct DevInfo {
    pub(crate) nr_hw_queues: u16,
    pub(crate) queue_depth: u16,
    pub(crate) state: u16,
    pub(crate) pad0: u16,
    pub(crate) max_io_buf_bytes: u32,
    pub(crate) dev_id: u32,
    pub(crate) ublksrv_pid: i32,
    pub(crate) pad1: u32,
    pub(crate) flags: u64,
    pub(crate) ublksrv_flags: u64,
    pub(crate) owner_uid: u32,
    pub(crate) owner_gid: u32,
    pub(crate) reserved1: u64,
    pub(crate) reserved2: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(crate) struct IoDesc {
    pub(crate) op_flags: u32,
    pub(crate) nr_sectors: u32,
    pub(crate) start_sector: u64,
    pub(crate) addr: u64,
}

impl IoDesc {
    pub(crate) fn op(&self) -> u8 {
        (self.op_flags & 0xff) as u8
    }
    pub(crate) fn flags(&self) -> u32 {
        self.op_flags & !0xff
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(crate) struct IoCmd {
    pub(crate) q_id: u16,
    pub(crate) tag: u16,
    pub(crate) result: i32,
    pub(crate) addr: u64,
}

impl IoCmd {
    /// The 16 bytes that ride in `sqe->cmd` for every ublk io command.
    pub(crate) fn encode(&self) -> [u8; 16] {
        // SAFETY: `IoCmd` is `repr(C)`, exactly 16 bytes, and has no padding holes.
        unsafe { std::mem::transmute(*self) }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct ParamBasic {
    pub(crate) attrs: u32,
    pub(crate) logical_bs_shift: u8,
    pub(crate) physical_bs_shift: u8,
    pub(crate) io_opt_shift: u8,
    pub(crate) io_min_shift: u8,
    pub(crate) max_sectors: u32,
    pub(crate) chunk_sectors: u32,
    pub(crate) dev_sectors: u64,
    pub(crate) virt_boundary_mask: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct ParamDiscard {
    pub(crate) discard_alignment: u32,
    pub(crate) discard_granularity: u32,
    pub(crate) max_discard_sectors: u32,
    pub(crate) max_write_zeroes_sectors: u32,
    pub(crate) max_discard_segments: u16,
    pub(crate) reserved0: u16,
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct ParamDmaAlign {
    pub(crate) alignment: u32,
    pub(crate) pad: [u8; 4],
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) struct Params {
    pub(crate) len: u32,
    pub(crate) types: u32,
    pub(crate) basic: ParamBasic,
    pub(crate) discard: ParamDiscard,
    /// `devt` (read-only) and `zoned`: type bits never set, but keep the uapi layout.
    devt_zoned: [u8; 48],
    pub(crate) dma: ParamDmaAlign,
    /// `seg`; the defaults are fine, so we never set `UBLK_PARAM_TYPE_SEGMENT`.
    seg: [u8; 16],
}

const PARAM_TYPE_BASIC: u32 = 1 << 0;
const PARAM_TYPE_DISCARD: u32 = 1 << 1;
const PARAM_TYPE_DMA_ALIGN: u32 = 1 << 4;

/// `UBLK_ATTR_FUA`. No `UBLK_ATTR_VOLATILE_CACHE`: no FLUSH from the block layer, a
/// completed write is durable, and `UBLK_IO_F_FUA` never matters here.
const ATTR_FUA: u32 = 1 << 3;

/// Device parameters: 4 KiB logical blocks, discard advertised, write-zeroes emulated as
/// writes.
///
/// Every page is a 4 KiB block now, whatever its kind, so there is one geometry for every
/// client device: `max_sectors` and `chunk_sectors` are one block, the block layer hands
/// over one whole block at a time and never straddles two, and discard has the same
/// granularity. A 4 MiB stripe is placement, not a transfer width, so it never appears
/// here.
pub(crate) fn params_for(size_bytes: u64) -> Params {
    const BLOCK_SECTORS: u32 = 8;
    Params {
        len: size_of::<Params>() as u32,
        types: PARAM_TYPE_BASIC | PARAM_TYPE_DISCARD | PARAM_TYPE_DMA_ALIGN,
        basic: ParamBasic {
            attrs: ATTR_FUA,
            logical_bs_shift: 12,
            physical_bs_shift: 12,
            io_opt_shift: 12,
            io_min_shift: 12,
            max_sectors: BLOCK_SECTORS,
            chunk_sectors: BLOCK_SECTORS,
            dev_sectors: size_bytes / 512,
            virt_boundary_mask: 0,
        },
        discard: ParamDiscard {
            discard_alignment: 0,
            discard_granularity: BLOCK_SECTORS * 512,
            max_discard_sectors: BLOCK_SECTORS,
            max_write_zeroes_sectors: 0,
            max_discard_segments: 1,
            reserved0: 0,
        },
        devt_zoned: [0; 48],
        dma: ParamDmaAlign {
            alignment: 4095,
            pad: [0; 4],
        },
        seg: [0; 16],
    }
}

/// The fabric device. A frame is one block, or one block and its trailer, so a request
/// must not be cut at a block boundary; `chunk_sectors` is two blocks, the largest frame
/// and its own alignment, so no request spans two frames. No discard: a delete is `TRIM`.
pub(crate) fn params_for_fabric(size_bytes: u64) -> Params {
    const FRAME_SECTORS: u32 = 16;
    Params {
        len: size_of::<Params>() as u32,
        types: PARAM_TYPE_BASIC | PARAM_TYPE_DMA_ALIGN,
        basic: ParamBasic {
            attrs: ATTR_FUA,
            logical_bs_shift: 12,
            physical_bs_shift: 12,
            io_opt_shift: 12,
            io_min_shift: 12,
            max_sectors: FRAME_SECTORS,
            chunk_sectors: FRAME_SECTORS,
            dev_sectors: size_bytes / 512,
            virt_boundary_mask: 0,
        },
        discard: ParamDiscard::default(),
        devt_zoned: [0; 48],
        dma: ParamDmaAlign {
            alignment: 4095,
            pad: [0; 4],
        },
        seg: [0; 16],
    }
}

/// Packs `struct ublk_auto_buf_reg` into an SQE `addr`.
pub(crate) fn auto_buf_reg(index: u16, flags: u8) -> u64 {
    (index as u64) | ((flags as u64) << 16)
}

pub(crate) fn char_dev_path(dev_id: u32) -> String {
    format!("/dev/ublkc{dev_id}")
}

pub(crate) fn block_dev_path(dev_id: u32) -> String {
    format!("/dev/ublkb{dev_id}")
}
