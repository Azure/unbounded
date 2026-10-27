//! Mirrors of `<linux/ublk_cmd.h>`.
//!
//! Only the parts the dataplane uses are defined. Sizes, offsets, and
//! ioctl encodings are checked against the header values in tests.

/// `struct ublksrv_ctrl_cmd` size.
pub(crate) const CTRL_CMD_SIZE: usize = 32;
/// `struct ublksrv_io_cmd` size.
pub(crate) const IO_CMD_SIZE: usize = 16;
/// `struct ublksrv_io_desc` size.
pub(crate) const IO_DESC_SIZE: usize = 24;
/// `struct ublksrv_ctrl_dev_info` size.
pub(crate) const DEV_INFO_SIZE: usize = 64;
/// `struct ublk_params` size.
pub(crate) const PARAMS_SIZE: usize = 112;

// Linux generic ioctl encoding (asm-generic/ioctl.h).
const IOC_NR_SHIFT: u32 = 0;
const IOC_TYPE_SHIFT: u32 = 8;
const IOC_SIZE_SHIFT: u32 = 16;
const IOC_DIR_SHIFT: u32 = 30;
const IOC_WRITE: u32 = 1;
const IOC_READ: u32 = 2;

const fn ioc(dir: u32, nr: u32, size: usize) -> u32 {
    (dir << IOC_DIR_SHIFT)
        | ((b'u' as u32) << IOC_TYPE_SHIFT)
        | (nr << IOC_NR_SHIFT)
        | ((size as u32) << IOC_SIZE_SHIFT)
}

const fn iowr(nr: u32, size: usize) -> u32 {
    ioc(IOC_READ | IOC_WRITE, nr, size)
}

const fn ior(nr: u32, size: usize) -> u32 {
    ioc(IOC_READ, nr, size)
}

/// `UBLK_U_CMD_GET_DEV_INFO`.
pub(crate) const U_CMD_GET_DEV_INFO: u32 = ior(0x02, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_ADD_DEV`.
pub(crate) const U_CMD_ADD_DEV: u32 = iowr(0x04, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_DEL_DEV`.
pub(crate) const U_CMD_DEL_DEV: u32 = iowr(0x05, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_START_DEV`.
pub(crate) const U_CMD_START_DEV: u32 = iowr(0x06, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_STOP_DEV`.
pub(crate) const U_CMD_STOP_DEV: u32 = iowr(0x07, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_SET_PARAMS`.
pub(crate) const U_CMD_SET_PARAMS: u32 = iowr(0x08, CTRL_CMD_SIZE);
/// `UBLK_U_CMD_DEL_DEV_ASYNC` (Linux 6.10 and later).
pub(crate) const U_CMD_DEL_DEV_ASYNC: u32 = ior(0x14, CTRL_CMD_SIZE);

/// `UBLK_U_IO_FETCH_REQ`.
pub(crate) const U_IO_FETCH_REQ: u32 = iowr(0x20, IO_CMD_SIZE);
/// `UBLK_U_IO_COMMIT_AND_FETCH_REQ`.
pub(crate) const U_IO_COMMIT_AND_FETCH_REQ: u32 = iowr(0x21, IO_CMD_SIZE);

/// `UBLK_F_CMD_IOCTL_ENCODE`.
pub(crate) const UBLK_F_CMD_IOCTL_ENCODE: u64 = 1 << 6;
/// `UBLK_F_USER_COPY`.
pub(crate) const UBLK_F_USER_COPY: u64 = 1 << 7;

/// `UBLK_ATTR_READ_ONLY`.
pub(crate) const UBLK_ATTR_READ_ONLY: u32 = 1 << 0;
/// `UBLK_PARAM_TYPE_BASIC`.
pub(crate) const UBLK_PARAM_TYPE_BASIC: u32 = 1 << 0;

/// `UBLK_IO_RES_OK`.
pub(crate) const UBLK_IO_RES_OK: i32 = 0;
/// `UBLK_IO_RES_ABORT`: the fetch completed because the queue is going away.
pub(crate) const UBLK_IO_RES_ABORT: i32 = -libc::ENODEV;

/// `UBLK_S_DEV_DEAD`.
pub(crate) const UBLK_S_DEV_DEAD: u16 = 0;
/// `UBLK_S_DEV_LIVE`.
pub(crate) const UBLK_S_DEV_LIVE: u16 = 1;

/// `UBLK_IO_OP_*` values in the low byte of `ublksrv_io_desc.op_flags`.
pub(crate) const UBLK_IO_OP_READ: u8 = 0;
pub(crate) const UBLK_IO_OP_WRITE: u8 = 1;
pub(crate) const UBLK_IO_OP_FLUSH: u8 = 2;
pub(crate) const UBLK_IO_OP_DISCARD: u8 = 3;
pub(crate) const UBLK_IO_OP_WRITE_SAME: u8 = 4;
pub(crate) const UBLK_IO_OP_WRITE_ZEROES: u8 = 5;

/// `UBLK_MAX_QUEUE_DEPTH`.
pub(crate) const UBLK_MAX_QUEUE_DEPTH: u16 = 4096;
/// `UBLK_MAX_NR_QUEUES`.
pub(crate) const UBLK_MAX_NR_QUEUES: u16 = 1 << UBLK_QID_BITS;

/// `UBLKSRV_CMD_BUF_OFFSET`: mmap offset of queue 0's descriptor array.
pub(crate) const UBLKSRV_CMD_BUF_OFFSET: i64 = 0;
/// `UBLKSRV_IO_BUF_OFFSET`: char device offset of the user-copy window.
pub(crate) const UBLKSRV_IO_BUF_OFFSET: u64 = 0x8000_0000;
/// `UBLK_IO_BUF_BITS`: bytes addressable within one request (32 MiB).
pub(crate) const UBLK_IO_BUF_BITS: u32 = 25;
/// `UBLK_TAG_OFF`.
pub(crate) const UBLK_TAG_OFF: u32 = UBLK_IO_BUF_BITS;
/// `UBLK_TAG_BITS`.
pub(crate) const UBLK_TAG_BITS: u32 = 16;
/// `UBLK_QID_OFF`.
pub(crate) const UBLK_QID_OFF: u32 = UBLK_TAG_OFF + UBLK_TAG_BITS;
/// `UBLK_QID_BITS`.
pub(crate) const UBLK_QID_BITS: u32 = 12;

/// Kernel-internal `ERESTARTSYS`. It can leak into CQE results of
/// interruptible control commands such as START_DEV.
pub(crate) const ERESTARTSYS: i32 = 512;

/// `struct ublksrv_ctrl_cmd`: the 32-byte payload of a control SQE.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct CtrlCmd {
    pub dev_id: u32,
    pub queue_id: u16,
    pub len: u16,
    pub addr: u64,
    pub data: u64,
    pub dev_path_len: u16,
    pub pad: u16,
    pub reserved: u32,
}

/// `struct ublksrv_ctrl_dev_info`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct DevInfo {
    pub nr_hw_queues: u16,
    pub queue_depth: u16,
    pub state: u16,
    pub pad0: u16,
    pub max_io_buf_bytes: u32,
    pub dev_id: u32,
    pub ublksrv_pid: i32,
    pub pad1: u32,
    pub flags: u64,
    /// Stored and returned by the driver, never interpreted. Used as a cookie.
    pub ublksrv_flags: u64,
    pub owner_uid: u32,
    pub owner_gid: u32,
    pub reserved1: u64,
    pub reserved2: u64,
}

/// `struct ublk_param_basic`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct ParamBasic {
    pub attrs: u32,
    pub logical_bs_shift: u8,
    pub physical_bs_shift: u8,
    pub io_opt_shift: u8,
    pub io_min_shift: u8,
    pub max_sectors: u32,
    pub chunk_sectors: u32,
    pub dev_sectors: u64,
    pub virt_boundary_mask: u64,
}

/// `struct ublk_param_discard`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct ParamDiscard {
    pub discard_alignment: u32,
    pub discard_granularity: u32,
    pub max_discard_sectors: u32,
    pub max_write_zeroes_sectors: u32,
    pub max_discard_segments: u16,
    pub reserved0: u16,
}

/// `struct ublk_param_devt`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct ParamDevt {
    pub char_major: u32,
    pub char_minor: u32,
    pub disk_major: u32,
    pub disk_minor: u32,
}

/// `struct ublk_param_zoned`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct ParamZoned {
    pub max_open_zones: u32,
    pub max_active_zones: u32,
    pub max_zone_append_sectors: u32,
    pub reserved: [u8; 20],
}

/// `struct ublk_params`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct Params {
    pub len: u32,
    pub types: u32,
    pub basic: ParamBasic,
    pub discard: ParamDiscard,
    pub devt: ParamDevt,
    pub zoned: ParamZoned,
}

/// `struct ublksrv_io_desc`: one tag's request, in the mmapped array.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct IoDesc {
    pub op_flags: u32,
    pub nr_sectors: u32,
    pub start_sector: u64,
    pub addr: u64,
}

/// `struct ublksrv_io_cmd`: the 16-byte payload of a queue SQE.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct IoCmd {
    pub q_id: u16,
    pub tag: u16,
    pub result: i32,
    pub addr: u64,
}

/// Returns the bytes of a plain-old-data value.
///
/// # Safety
///
/// `T` must be `#[repr(C)]` with no padding and contain only integers.
unsafe fn pod_bytes<T: Copy, const N: usize>(value: &T) -> [u8; N] {
    debug_assert_eq!(std::mem::size_of::<T>(), N);
    let mut out = [0_u8; N];
    // SAFETY: the caller guarantees `T` is padding-free POD of size `N`, so all
    // `N` source bytes are initialized, and `out` is a distinct `N`-byte buffer.
    unsafe {
        std::ptr::copy_nonoverlapping(std::ptr::from_ref(value).cast::<u8>(), out.as_mut_ptr(), N);
    }
    out
}

/// Builds a plain-old-data value from bytes.
///
/// # Safety
///
/// `T` must be `#[repr(C)]` with no padding, contain only integers (every bit
/// pattern valid), and have size `N`.
unsafe fn pod_from<T: Copy + Default, const N: usize>(bytes: &[u8; N]) -> T {
    debug_assert_eq!(std::mem::size_of::<T>(), N);
    let mut out = T::default();
    // SAFETY: the caller guarantees `T` has size `N` and accepts any bit pattern.
    unsafe {
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), std::ptr::from_mut(&mut out).cast::<u8>(), N);
    }
    out
}

impl CtrlCmd {
    pub(crate) fn to_bytes(self) -> [u8; CTRL_CMD_SIZE] {
        // SAFETY: CtrlCmd is repr(C), padding-free (size test), integers only.
        unsafe { pod_bytes(&self) }
    }
}

impl DevInfo {
    pub(crate) fn to_bytes(self) -> [u8; DEV_INFO_SIZE] {
        // SAFETY: DevInfo is repr(C), padding-free (size test), integers only.
        unsafe { pod_bytes(&self) }
    }

    pub(crate) fn from_bytes(bytes: &[u8; DEV_INFO_SIZE]) -> Self {
        // SAFETY: DevInfo is repr(C), padding-free, and every bit pattern is valid.
        unsafe { pod_from(bytes) }
    }
}

impl Params {
    pub(crate) fn to_bytes(self) -> [u8; PARAMS_SIZE] {
        // SAFETY: Params is repr(C), padding-free (size and offset tests),
        // integers and byte arrays only.
        unsafe { pod_bytes(&self) }
    }

    /// Read-only basic parameters for a device of `size_bytes`, which must be
    /// a multiple of 512. Logical blocks are 512 bytes; physical, optimal, and
    /// minimum I/O sizes are 4096.
    pub(crate) fn read_only_basic(size_bytes: u64, max_io_bytes: u32) -> Self {
        Self {
            len: PARAMS_SIZE as u32,
            types: UBLK_PARAM_TYPE_BASIC,
            basic: ParamBasic {
                attrs: UBLK_ATTR_READ_ONLY,
                logical_bs_shift: 9,
                physical_bs_shift: 12,
                io_opt_shift: 12,
                io_min_shift: 12,
                max_sectors: max_io_bytes >> 9,
                dev_sectors: size_bytes >> 9,
                ..ParamBasic::default()
            },
            ..Self::default()
        }
    }
}

impl IoDesc {
    pub(crate) fn to_bytes(self) -> [u8; IO_DESC_SIZE] {
        // SAFETY: IoDesc is repr(C), padding-free (size test), integers only.
        unsafe { pod_bytes(&self) }
    }

    /// The `UBLK_IO_OP_*` value.
    pub(crate) fn op(&self) -> u8 {
        (self.op_flags & 0xff) as u8
    }
}

impl IoCmd {
    pub(crate) fn to_bytes(self) -> [u8; IO_CMD_SIZE] {
        // SAFETY: IoCmd is repr(C), padding-free (size test), integers only.
        unsafe { pod_bytes(&self) }
    }
}

/// Char device offset for user copy of byte `offset` of request `tag` on
/// queue `q` (the kernel's `ublk_pos`).
pub(crate) fn ublk_pos(q: u16, tag: u16, offset: u32) -> u64 {
    debug_assert!(u64::from(offset) < 1 << UBLK_IO_BUF_BITS);
    UBLKSRV_IO_BUF_OFFSET
        + (u64::from(q) << UBLK_QID_OFF)
        + (u64::from(tag) << UBLK_TAG_OFF)
        + u64::from(offset)
}

/// System page size.
pub(crate) fn page_size() -> usize {
    // SAFETY: sysconf has no memory-safety preconditions.
    let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    usize::try_from(size)
        .ok()
        .filter(|size| *size > 0)
        .unwrap_or(4096)
}

/// Rounds `len` up to a multiple of `page`.
pub(crate) fn round_up(len: usize, page: usize) -> usize {
    len.div_ceil(page) * page
}

/// Length of one queue's descriptor mapping for `depth` tags.
pub(crate) fn descriptor_len(depth: u16) -> usize {
    round_up(usize::from(depth) * IO_DESC_SIZE, page_size())
}

/// mmap offset of queue `q`'s descriptor array. The kernel strides queues by
/// the array size for `UBLK_MAX_QUEUE_DEPTH`, not the configured depth (see
/// `ublk_ch_mmap` and `ublk_max_cmd_buf_size`).
pub(crate) fn descriptor_offset(q: u16) -> i64 {
    UBLKSRV_CMD_BUF_OFFSET + i64::from(q) * descriptor_len(UBLK_MAX_QUEUE_DEPTH) as i64
}

#[cfg(test)]
mod tests {
    use std::mem::{offset_of, size_of};

    use super::*;

    #[test]
    fn struct_sizes_match_header() {
        assert_eq!(size_of::<CtrlCmd>(), CTRL_CMD_SIZE);
        assert_eq!(size_of::<DevInfo>(), DEV_INFO_SIZE);
        assert_eq!(size_of::<Params>(), PARAMS_SIZE);
        assert_eq!(size_of::<IoDesc>(), IO_DESC_SIZE);
        assert_eq!(size_of::<IoCmd>(), IO_CMD_SIZE);
        assert_eq!(size_of::<ParamBasic>(), 32);
        assert_eq!(size_of::<ParamDiscard>(), 20);
        assert_eq!(size_of::<ParamDevt>(), 16);
        assert_eq!(size_of::<ParamZoned>(), 32);
    }

    #[test]
    fn field_offsets_match_header() {
        assert_eq!(offset_of!(CtrlCmd, addr), 8);
        assert_eq!(offset_of!(CtrlCmd, data), 16);
        assert_eq!(offset_of!(CtrlCmd, dev_path_len), 24);
        assert_eq!(offset_of!(DevInfo, max_io_buf_bytes), 8);
        assert_eq!(offset_of!(DevInfo, dev_id), 12);
        assert_eq!(offset_of!(DevInfo, flags), 24);
        assert_eq!(offset_of!(DevInfo, ublksrv_flags), 32);
        assert_eq!(offset_of!(DevInfo, owner_uid), 40);
        assert_eq!(offset_of!(Params, basic), 8);
        assert_eq!(offset_of!(Params, discard), 40);
        assert_eq!(offset_of!(Params, devt), 60);
        assert_eq!(offset_of!(Params, zoned), 76);
        assert_eq!(offset_of!(ParamBasic, max_sectors), 8);
        assert_eq!(offset_of!(ParamBasic, dev_sectors), 16);
        assert_eq!(offset_of!(IoDesc, start_sector), 8);
        assert_eq!(offset_of!(IoDesc, addr), 16);
        assert_eq!(offset_of!(IoCmd, result), 4);
        assert_eq!(offset_of!(IoCmd, addr), 8);
    }

    #[test]
    fn ioctl_encodings_match_header() {
        // Values computed from _IOR/_IOWR('u', nr, struct) in <linux/ublk_cmd.h>.
        assert_eq!(U_CMD_GET_DEV_INFO, 0x8020_7502);
        assert_eq!(U_CMD_ADD_DEV, 0xc020_7504);
        assert_eq!(U_CMD_DEL_DEV, 0xc020_7505);
        assert_eq!(U_CMD_START_DEV, 0xc020_7506);
        assert_eq!(U_CMD_STOP_DEV, 0xc020_7507);
        assert_eq!(U_CMD_SET_PARAMS, 0xc020_7508);
        assert_eq!(U_CMD_DEL_DEV_ASYNC, 0x8020_7514);
        assert_eq!(U_IO_FETCH_REQ, 0xc010_7520);
        assert_eq!(U_IO_COMMIT_AND_FETCH_REQ, 0xc010_7521);
    }

    #[test]
    fn ublk_pos_layout() {
        assert_eq!(ublk_pos(0, 0, 0), 0x8000_0000);
        assert_eq!(ublk_pos(0, 1, 0), 0x8000_0000 + (1 << 25));
        assert_eq!(ublk_pos(1, 0, 0), 0x8000_0000 + (1 << 41));
        assert_eq!(ublk_pos(2, 3, 5), 0x8000_0000 + (2 << 41) + (3 << 25) + 5);
        assert_eq!(UBLK_MAX_NR_QUEUES, 4096);
    }

    #[test]
    fn descriptor_offsets_stride_by_max_depth() {
        let page = page_size();
        assert_eq!(descriptor_offset(0), 0);
        let stride = round_up(4096 * 24, page) as i64;
        assert_eq!(descriptor_offset(1), stride);
        assert_eq!(descriptor_offset(3), 3 * stride);
        assert_eq!(descriptor_len(1), page);
        assert_eq!(descriptor_len(128), round_up(128 * 24, page));
    }

    #[test]
    fn read_only_basic_params() {
        let params = Params::read_only_basic(1 << 30, 1 << 20);
        assert_eq!(params.len, 112);
        assert_eq!(params.types, UBLK_PARAM_TYPE_BASIC);
        assert_eq!(params.basic.attrs, UBLK_ATTR_READ_ONLY);
        assert_eq!(params.basic.logical_bs_shift, 9);
        assert_eq!(params.basic.physical_bs_shift, 12);
        assert_eq!(params.basic.io_opt_shift, 12);
        assert_eq!(params.basic.io_min_shift, 12);
        assert_eq!(params.basic.max_sectors, 2048);
        assert_eq!(params.basic.dev_sectors, 1 << 21);
        let bytes = params.to_bytes();
        assert_eq!(&bytes[0..4], &112_u32.to_ne_bytes());
        assert_eq!(&bytes[24..32], &(1_u64 << 21).to_ne_bytes());
    }

    #[test]
    fn byte_round_trips() {
        let info = DevInfo {
            nr_hw_queues: 2,
            queue_depth: 64,
            dev_id: 9,
            ublksrv_flags: 0xfeed,
            ..DevInfo::default()
        };
        assert_eq!(DevInfo::from_bytes(&info.to_bytes()), info);
        let cmd = IoCmd {
            q_id: 1,
            tag: 2,
            result: -5,
            addr: 0,
        };
        let bytes = cmd.to_bytes();
        assert_eq!(&bytes[0..2], &1_u16.to_ne_bytes());
        assert_eq!(&bytes[2..4], &2_u16.to_ne_bytes());
        assert_eq!(&bytes[4..8], &(-5_i32).to_ne_bytes());
        let ctrl = CtrlCmd {
            dev_id: u32::MAX,
            queue_id: 0xffff,
            ..CtrlCmd::default()
        };
        assert_eq!(&ctrl.to_bytes()[0..6], &[0xff; 6]);
        let desc = IoDesc {
            op_flags: 0x1234_5602,
            ..IoDesc::default()
        };
        assert_eq!(desc.op(), UBLK_IO_OP_FLUSH);
        assert_eq!(desc.to_bytes().len(), 24);
    }
}
