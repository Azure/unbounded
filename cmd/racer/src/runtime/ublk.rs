//! ublk uapi and the control-plane ring.
//!
//! Hand-written bindings for `include/uapi/linux/ublk_cmd.h`. Control commands are
//! `uring_cmd`s carrying a 32-byte `ublksrv_ctrl_cmd` in 128-byte SQEs, each synchronous.

use std::io;
use std::os::fd::{AsRawFd, OwnedFd};
use std::path::Path;

use io_uring::{IoUring, cqueue, opcode, squeue, types};

use super::sys;
use crate::config::Class;

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
/// request: zero copy for 4 MiB pages, a copy for the 4 KiB pages the allocator checksums.
pub(super) const F_USER_COPY: u64 = 1 << 7;
/// `UBLK_F_AUTO_BUF_REG`: the kernel registers the request buffer into our ring as part of
/// FETCH. Unlike `UBLK_F_SUPPORT_ZERO_COPY` (1 << 0), it costs no extra uring_cmds per IO.
pub(super) const F_AUTO_BUF_REG: u64 = 1 << 11;

pub(super) const IO_RES_OK: i32 = 0;

pub(super) const OP_READ: u8 = 0;
pub(super) const OP_WRITE: u8 = 1;
pub(super) const OP_DISCARD: u8 = 3;

/// Set when auto buffer registration fell back and we must register by hand.
pub(super) const IO_F_NEED_REG_BUF: u32 = 1 << 17;

pub(super) const AUTO_BUF_REG_FALLBACK: u8 = 1 << 0;

const MAX_QUEUE_DEPTH: u32 = 4096;

/// `UBLKSRV_IO_BUF_OFFSET`, with `(q_id, tag)` packed into the bits above it.
const IO_BUF_OFFSET: u64 = 0x8000_0000;
const TAG_OFF: u32 = 25;
const QID_OFF: u32 = 41;

/// File offset of a request's payload within `/dev/ublkcN`, for `USER_COPY`.
pub(super) fn buf_offset(q_id: u16, tag: u16, off: usize) -> u64 {
    IO_BUF_OFFSET + ((q_id as u64) << QID_OFF) + ((tag as u64) << TAG_OFF) + off as u64
}

/// Kernel's per-queue io_desc buffer stride; see `ublk_max_cmd_buf_size()`.
pub(super) fn cmd_buf_size() -> usize {
    (MAX_QUEUE_DEPTH as usize * size_of::<IoDesc>()).next_multiple_of(sys::page_size())
}

// --- structs ---

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(super) struct CtrlCmd {
    pub(super) dev_id: u32,
    pub(super) queue_id: u16,
    pub(super) len: u16,
    pub(super) addr: u64,
    pub(super) data: [u64; 1],
    pub(super) dev_path_len: u16,
    pub(super) pad: u16,
    pub(super) reserved: u32,
}

impl DevInfo {
    /// A device request at the minor `dev_id`. The node asks for the number rather than
    /// taking what the kernel hands out: the path is `/dev/ublkb<dev_id>`, and the control
    /// plane published it before this process started.
    pub(super) fn new(dev_id: u32, nr_hw_queues: u16, queue_depth: u16, flags: u64) -> DevInfo {
        DevInfo {
            nr_hw_queues,
            queue_depth,
            dev_id,
            max_io_buf_bytes: super::MAX_IO_BYTES as u32,
            flags,
            ..Default::default()
        }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub(super) struct DevInfo {
    pub(super) nr_hw_queues: u16,
    pub(super) queue_depth: u16,
    pub(super) state: u16,
    pub(super) pad0: u16,
    pub(super) max_io_buf_bytes: u32,
    pub(super) dev_id: u32,
    pub(super) ublksrv_pid: i32,
    pub(super) pad1: u32,
    pub(super) flags: u64,
    pub(super) ublksrv_flags: u64,
    pub(super) owner_uid: u32,
    pub(super) owner_gid: u32,
    pub(super) reserved1: u64,
    pub(super) reserved2: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(super) struct IoDesc {
    pub(super) op_flags: u32,
    pub(super) nr_sectors: u32,
    pub(super) start_sector: u64,
    pub(super) addr: u64,
}

impl IoDesc {
    pub(super) fn op(&self) -> u8 {
        (self.op_flags & 0xff) as u8
    }
    pub(super) fn flags(&self) -> u32 {
        self.op_flags & !0xff
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub(super) struct IoCmd {
    pub(super) q_id: u16,
    pub(super) tag: u16,
    pub(super) result: i32,
    pub(super) addr: u64,
}

impl IoCmd {
    /// The 16 bytes that ride in `sqe->cmd` for every ublk io command.
    pub(super) fn encode(&self) -> [u8; 16] {
        // SAFETY: `IoCmd` is `repr(C)`, exactly 16 bytes, and has no padding holes.
        unsafe { std::mem::transmute(*self) }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) struct ParamBasic {
    pub(super) attrs: u32,
    pub(super) logical_bs_shift: u8,
    pub(super) physical_bs_shift: u8,
    pub(super) io_opt_shift: u8,
    pub(super) io_min_shift: u8,
    pub(super) max_sectors: u32,
    pub(super) chunk_sectors: u32,
    pub(super) dev_sectors: u64,
    pub(super) virt_boundary_mask: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) struct ParamDiscard {
    pub(super) discard_alignment: u32,
    pub(super) discard_granularity: u32,
    pub(super) max_discard_sectors: u32,
    pub(super) max_write_zeroes_sectors: u32,
    pub(super) max_discard_segments: u16,
    pub(super) reserved0: u16,
}

#[repr(C)]
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub(super) struct ParamDmaAlign {
    pub(super) alignment: u32,
    pub(super) pad: [u8; 4],
}

#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) struct Params {
    pub(super) len: u32,
    pub(super) types: u32,
    pub(super) basic: ParamBasic,
    pub(super) discard: ParamDiscard,
    /// `devt` (read-only) and `zoned`: type bits never set, but keep the uapi layout.
    devt_zoned: [u8; 48],
    pub(super) dma: ParamDmaAlign,
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
/// Transfer limits follow the allocator pages behind the device. With one page size,
/// `max_sectors` and `chunk_sectors` are that page, so the block layer hands over one
/// whole page at a time and never straddles two. A device built from both cannot state a
/// single alignment, so it takes the largest transfer and no chunk at all, and the
/// consumer path cuts each request on the pages it actually crosses. Discard is a page as
/// well, except when mixed: a granularity of 4 MiB would swallow every 4 KiB trim, so the
/// smaller one is advertised and a partial trim of a 4 MiB page is refused instead.
pub(super) fn params_for(size_bytes: u64, class: Class) -> Params {
    let huge = class == Class::Huge;
    let page_sectors: u32 = if huge { 8192 } else { 8 };
    let (max_sectors, chunk_sectors) = match class {
        Class::Mixed => (8192, 0),
        _ => (page_sectors, page_sectors),
    };
    Params {
        len: size_of::<Params>() as u32,
        types: PARAM_TYPE_BASIC | PARAM_TYPE_DISCARD | PARAM_TYPE_DMA_ALIGN,
        basic: ParamBasic {
            attrs: ATTR_FUA,
            logical_bs_shift: 12,
            physical_bs_shift: 12,
            io_opt_shift: if huge { 22 } else { 12 },
            io_min_shift: 12,
            max_sectors,
            chunk_sectors,
            dev_sectors: size_bytes / 512,
            virt_boundary_mask: 0,
        },
        discard: ParamDiscard {
            discard_alignment: 0,
            discard_granularity: page_sectors * 512,
            max_discard_sectors: max_sectors,
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

/// The fabric device. A request covers 1, 2, or up to 1024 blocks depending on the op, so
/// it must not be split on page boundaries; `chunk_sectors` is one 4 MiB page, the largest
/// frame and its own alignment, so no request spans two. No discard: a delete is `TRIM`.
pub(super) fn params_for_fabric(size_bytes: u64) -> Params {
    const HUGE_SECTORS: u32 = 8192;
    Params {
        len: size_of::<Params>() as u32,
        types: PARAM_TYPE_BASIC | PARAM_TYPE_DMA_ALIGN,
        basic: ParamBasic {
            attrs: ATTR_FUA,
            logical_bs_shift: 12,
            physical_bs_shift: 12,
            io_opt_shift: 12,
            io_min_shift: 12,
            max_sectors: HUGE_SECTORS,
            chunk_sectors: HUGE_SECTORS,
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

// --- control ring ---
/// Synchronous owner of `/dev/ublk-control`. Lives on the control thread only.
pub(super) struct Control {
    ring: IoUring<squeue::Entry128, cqueue::Entry>,
    fd: OwnedFd,
    pub(super) features: u64,
}

impl Control {
    pub(super) fn open() -> io::Result<Control> {
        let fd = sys::open_flags(
            Path::new("/dev/ublk-control"),
            libc::O_RDWR | libc::O_CLOEXEC,
        )?;
        let ring = IoUring::<squeue::Entry128, cqueue::Entry>::builder().build(8)?;
        let mut c = Control {
            ring,
            fd,
            features: 0,
        };
        c.features = c.get_features()?;
        Ok(c)
    }

    fn exec(&mut self, op: u32, cmd: &CtrlCmd) -> io::Result<i32> {
        let mut buf = [0u8; 80];
        unsafe {
            std::ptr::copy_nonoverlapping(
                cmd as *const CtrlCmd as *const u8,
                buf.as_mut_ptr(),
                size_of::<CtrlCmd>(),
            )
        };
        let e = opcode::UringCmd80::new(types::Fd(self.fd.as_raw_fd()), op)
            .cmd(buf)
            .build()
            .user_data(0);
        unsafe { self.ring.submission().push(&e) }
            .map_err(|_| io::Error::from_raw_os_error(libc::ENOSPC))?;
        self.ring.submit_and_wait(1)?;
        let cqe = self.ring.completion().next().expect("cqe");
        if cqe.result() < 0 {
            return Err(io::Error::from_raw_os_error(-cqe.result()));
        }
        Ok(cqe.result())
    }

    fn get_features(&mut self) -> io::Result<u64> {
        let mut out: u64 = 0;
        let cmd = CtrlCmd {
            dev_id: u32::MAX,
            queue_id: u16::MAX,
            len: 8,
            addr: &mut out as *mut u64 as u64,
            ..Default::default()
        };
        self.exec(CMD_GET_FEATURES, &cmd)?;
        Ok(out)
    }

    pub(super) fn require(&self, want: u64) -> io::Result<()> {
        let missing = want & !self.features;
        if missing != 0 {
            return Err(io::Error::other(format!(
                "ublk driver missing required features {missing:#x} (have {:#x})",
                self.features
            )));
        }
        Ok(())
    }

    pub(super) fn add_dev(&mut self, info: &mut DevInfo) -> io::Result<u32> {
        let cmd = CtrlCmd {
            dev_id: info.dev_id,
            queue_id: u16::MAX,
            len: size_of::<DevInfo>() as u16,
            addr: info as *mut DevInfo as u64,
            ..Default::default()
        };
        self.exec(CMD_ADD_DEV, &cmd)?;
        Ok(info.dev_id)
    }

    pub(super) fn set_params(&mut self, dev_id: u32, p: &Params) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of::<Params>() as u16,
            addr: p as *const Params as u64,
            ..Default::default()
        };
        self.exec(CMD_SET_PARAMS, &cmd).map(|_| ())
    }

    /// CPUs bound to queue `q`. Index in `data[0]`, `cpumask` of `len` bytes into `addr`.
    pub(super) fn queue_affinity(&mut self, dev_id: u32, q: u16) -> io::Result<Vec<usize>> {
        let mut mask = [0u64; 16];
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of_val(&mask) as u16,
            addr: mask.as_mut_ptr() as u64,
            data: [q as u64],
            ..Default::default()
        };
        self.exec(CMD_GET_QUEUE_AFFINITY, &cmd)?;
        Ok((0..mask.len() * 64)
            .filter(|b| mask[b / 64] & (1 << (b % 64)) != 0)
            .collect())
    }

    pub(super) fn start_dev(&mut self, dev_id: u32, pid: i32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            data: [pid as u64],
            ..Default::default()
        };
        self.exec(CMD_START_DEV, &cmd).map(|_| ())
    }

    pub(super) fn stop_dev(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_STOP_DEV, &cmd).map(|_| ())
    }

    pub(super) fn del_dev(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_DEL_DEV, &cmd).map(|_| ())
    }

    /// Ask for `dev_id` to go away without waiting for it.
    ///
    /// [`del_dev`](Self::del_dev) does not return until the minor is free, and a minor is
    /// only free once everyone who opened the block device behind it has closed it. A
    /// consumer that is still holding a dead export would therefore park this thread in
    /// the kernel with no way out. This asks for the same removal and comes straight
    /// back, leaving the minor to be freed whenever the last holder lets go.
    pub(super) fn del_dev_async(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_DEL_DEV_ASYNC, &cmd).map(|_| ())
    }

    /// What the kernel holds at `dev_id`, whoever put it there. `ENODEV` if the minor is
    /// free.
    pub(super) fn dev_info(&mut self, dev_id: u32) -> io::Result<DevInfo> {
        let mut info = DevInfo::default();
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of::<DevInfo>() as u16,
            addr: &mut info as *mut DevInfo as u64,
            ..Default::default()
        };
        self.exec(CMD_GET_DEV_INFO, &cmd)?;
        Ok(info)
    }
}

/// Packs `struct ublk_auto_buf_reg` into an SQE `addr`.
pub(super) fn auto_buf_reg(index: u16, flags: u8) -> u64 {
    (index as u64) | ((flags as u64) << 16)
}

pub(super) fn char_dev_path(dev_id: u32) -> String {
    format!("/dev/ublkc{dev_id}")
}

pub(super) fn block_dev_path(dev_id: u32) -> String {
    format!("/dev/ublkb{dev_id}")
}
