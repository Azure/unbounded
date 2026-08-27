//! What crosses the ring seam.
//!
//! These are the node's own submission and completion types, not the io_uring crate's.
//! That is deliberate. A submission is a request the node makes of a kernel, and the
//! simulator is a kernel: it has to read a submission, not merely carry one. The io_uring
//! crate's entry is an opaque wrapper around the raw SQE, so a simulator handed one could
//! only guess at it. An [`Sqe`] says what it wants, the real ring turns it back into the
//! bytes the kernel expects, and the simulated ring answers it directly.
//!
//! The set of operations here is closed on purpose: it is exactly what the node submits,
//! and adding one is a decision, not an accident.

use std::os::fd::RawFd;
use std::time::Duration;

/// A deadline, in the layout the kernel's timer operations read.
///
/// Laid out as `struct __kernel_timespec` because a timer carries the address of one of
/// these rather than a copy: the kernel reads it long after the submission was built, so
/// it has to stay where the caller put it. That is why an op slot owns one.
#[repr(C)]
#[derive(Copy, Clone, Default)]
pub(crate) struct Timespec {
    pub(crate) sec: i64,
    pub(crate) nsec: i64,
}

impl Timespec {
    pub(crate) fn from_duration(d: Duration) -> Timespec {
        Timespec {
            sec: d.as_secs() as i64,
            nsec: d.subsec_nanos() as i64,
        }
    }

    /// The whole deadline in microseconds, which is the unit virtual time runs in.
    #[allow(dead_code)] // Read by the simulated ring, once it arms the timers it is handed.
    pub(crate) fn as_micros(&self) -> u64 {
        (self.sec.max(0) as u64) * 1_000_000 + (self.nsec.max(0) as u64) / 1_000
    }
}

/// What one submission asks for.
///
/// Every file here is a fixed-file index: the node registers its descriptors up front and
/// never submits a raw one on the hot path, so the seam does not have to carry one.
///
/// Buffers are the other way round, and deliberately so. Most transfers name a
/// registered-buffer index, because the memory is registered anyway and the guest pages a
/// request arrives with are only ever reachable that way. But a registered buffer reaches
/// the kernel as an `ITER_BVEC` iterator, and `ublk_drv` refuses one:
/// `ublk_check_and_get_req()` answers `EACCES` unless `user_backed_iter()` holds. So the
/// copy between a request's guest pages and a worker's buffer, which is a `pread` or
/// `pwrite` on `/dev/ublkcN`, has to name its buffer by address. That is what [`Op::Read`]
/// and [`Op::Write`] are for, and the only thing they are for.
#[derive(Copy, Clone)]
pub(crate) enum Op {
    /// Read `len` bytes at `offset` into an ordinary, user-backed buffer.
    ///
    /// The buffer's memory may well be registered; naming it by address rather than by
    /// index is what makes the iterator the kernel builds a user-backed one.
    Read {
        file: u32,
        buf: *mut u8,
        len: u32,
        offset: u64,
    },
    /// Write `len` bytes at `offset` out of an ordinary, user-backed buffer.
    Write {
        file: u32,
        buf: *const u8,
        len: u32,
        offset: u64,
    },
    /// Read `len` bytes at `offset` into a registered buffer.
    ReadFixed {
        file: u32,
        buf: *mut u8,
        len: u32,
        buf_index: u16,
        offset: u64,
    },
    /// Write `len` bytes at `offset` out of a registered buffer.
    ///
    /// `dsync` is per-write FUA on an `O_DIRECT` device: the completion means stable media.
    /// There is no flush operation because there is never anything buffered to flush.
    WriteFixed {
        file: u32,
        buf: *const u8,
        len: u32,
        buf_index: u16,
        offset: u64,
        dsync: bool,
    },
    /// Complete once, `ts` from now.
    Timeout { ts: *const Timespec },
    /// Cancel the submission linked ahead of this one if it outlasts `ts`.
    LinkTimeout { ts: *const Timespec },
    /// A driver command on a registered file, carrying a sixteen byte payload.
    UringCmd16 {
        file: u32,
        cmd_op: u32,
        cmd: [u8; 16],
        addr: Option<u64>,
    },
    /// Post `data` as a completion on another ring, waking whoever is blocked on it.
    MsgRingData { fd: RawFd, data: u64 },
}

/// One submission.
#[derive(Copy, Clone)]
pub(crate) struct Sqe {
    pub(crate) op: Op,
    pub(crate) user_data: u64,
    /// Whether the next submission is linked to this one, and so must land with it.
    pub(crate) link: bool,
}

impl Sqe {
    pub(crate) fn new(op: Op, user_data: u64) -> Sqe {
        Sqe {
            op,
            user_data,
            link: false,
        }
    }

    /// Links the submission that follows to this one.
    pub(crate) fn linked(mut self) -> Sqe {
        self.link = true;
        self
    }
}

/// One completion.
///
/// The kernel's completion carries flags as well; nothing here reads them, so they do not
/// cross the seam.
#[derive(Copy, Clone)]
pub(crate) struct Cqe {
    pub(crate) user_data: u64,
    pub(crate) result: i32,
}
