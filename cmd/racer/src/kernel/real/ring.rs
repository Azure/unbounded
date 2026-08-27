//! The real ring: io_uring.

use std::io;
use std::os::fd::{AsRawFd, RawFd};

use io_uring::{IoUring, cqueue, opcode, squeue, types};

use crate::kernel::ring::{Cqe, Op, Sqe, Timespec};

/// `IORING_ENTER_GETEVENTS`: reap without submitting.
const ENTER_GETEVENTS: u32 = 1;

/// Per-write FUA on an `O_DIRECT` device.
const RWF_DSYNC: i32 = 0x2;

/// The node's timespec and the crate's are both `struct __kernel_timespec`, which is what
/// makes handing one address to the other sound. If that ever stops being true this stops
/// compiling rather than starting to lie.
const _: () = assert!(size_of::<Timespec>() == size_of::<types::Timespec>());
const _: () = assert!(align_of::<Timespec>() == align_of::<types::Timespec>());

pub(crate) struct Ring {
    ring: IoUring,
}

/// Turns a submission back into the bytes the kernel reads.
fn build(e: &Sqe) -> squeue::Entry {
    let s = match e.op {
        Op::Read {
            file,
            buf,
            len,
            offset,
        } => opcode::Read::new(types::Fixed(file), buf, len)
            .offset(offset)
            .build(),
        Op::Write {
            file,
            buf,
            len,
            offset,
        } => opcode::Write::new(types::Fixed(file), buf, len)
            .offset(offset)
            .build(),
        Op::ReadFixed {
            file,
            buf,
            len,
            buf_index,
            offset,
        } => opcode::ReadFixed::new(types::Fixed(file), buf, len, buf_index)
            .offset(offset)
            .build(),
        Op::WriteFixed {
            file,
            buf,
            len,
            buf_index,
            offset,
            dsync,
        } => opcode::WriteFixed::new(types::Fixed(file), buf, len, buf_index)
            .offset(offset)
            .rw_flags(if dsync { RWF_DSYNC } else { 0 })
            .build(),
        // SAFETY: the caller owns the timespec and keeps it alive past the completion.
        Op::Timeout { ts } => opcode::Timeout::new(ts as *const types::Timespec).build(),
        Op::LinkTimeout { ts } => opcode::LinkTimeout::new(ts as *const types::Timespec).build(),
        Op::UringCmd16 {
            file,
            cmd_op,
            cmd,
            addr,
        } => opcode::UringCmd16::new(types::Fixed(file), cmd_op)
            .cmd(cmd)
            .addr(addr)
            .build(),
        Op::MsgRingData { fd, data } => {
            opcode::MsgRingData::new(types::Fd(fd), 0, data, None).build()
        }
    };
    let s = s.user_data(e.user_data);
    if e.link {
        s.flags(squeue::Flags::IO_LINK)
    } else {
        s
    }
}

pub(crate) fn open(entries: u32, cq_entries: u32, polled: bool) -> io::Result<Ring> {
    let mut b = IoUring::builder();
    if polled {
        b.setup_single_issuer()
            .setup_defer_taskrun()
            .setup_taskrun_flag()
            .setup_submit_all();
    }
    Ok(Ring {
        ring: b.setup_cqsize(cq_entries).build(entries)?,
    })
}

impl Ring {
    pub(crate) fn push(&self, e: &Sqe) -> bool {
        // SAFETY: one issuer, and that is this thread.
        let mut sq = unsafe { self.ring.submission_shared() };
        unsafe { sq.push(&build(e)) }.is_ok()
    }

    /// Room left in the submission queue, in submissions.
    pub(crate) fn room(&self) -> usize {
        let sq = unsafe { self.ring.submission_shared() };
        sq.capacity() - sq.len()
    }

    pub(crate) fn is_empty(&self) -> bool {
        unsafe { self.ring.submission_shared() }.is_empty()
    }

    pub(crate) fn submit(&self) {
        let _ = self.ring.submitter().submit();
    }

    /// Whether the kernel has completions waiting that this thread has not been told about.
    pub(crate) fn taskrun(&self) -> bool {
        unsafe { self.ring.submission_shared() }.taskrun()
    }

    /// Collects completions the kernel has queued without submitting anything.
    pub(crate) fn get_events(&self) {
        let _ = unsafe {
            self.ring
                .submitter()
                .enter::<()>(0, 0, ENTER_GETEVENTS, None)
        };
    }

    pub(crate) fn drain(&self, out: &mut Vec<Cqe>) {
        let mut cq = unsafe { self.ring.completion_shared() };
        cq.sync();
        out.extend(cq.map(|c: cqueue::Entry| Cqe {
            user_data: c.user_data(),
            result: c.result(),
        }));
    }

    /// Submits and blocks for one completion, or until `ts` runs out.
    pub(crate) fn wait(&self, ts: &Timespec) {
        // SAFETY: layout-checked above, and `ts` outlives the call.
        let ts = unsafe { &*(ts as *const Timespec as *const types::Timespec) };
        let args = types::SubmitArgs::new().timespec(ts);
        let _ = self.ring.submitter().submit_with_args(1, &args);
    }

    pub(crate) fn register_files_sparse(&self, n: u32) -> io::Result<()> {
        self.ring.submitter().register_files_sparse(n)
    }

    pub(crate) fn register_files_update(
        &self,
        off: u32,
        fds: &[crate::kernel::FileRef],
    ) -> io::Result<()> {
        // A real ring takes descriptors; a simulated file never reaches one.
        let fds: Vec<RawFd> = fds
            .iter()
            .map(|f| match f {
                crate::kernel::FileRef::Real(fd) => *fd,
                crate::kernel::FileRef::Sim(_) => {
                    panic!("racer: a simulated file registered with a real ring")
                }
            })
            .collect();
        let fds = &fds[..];
        self.ring.submitter().register_files_update(off, fds)?;
        Ok(())
    }

    pub(crate) fn register_buffers_sparse(&self, n: u32) -> io::Result<()> {
        self.ring.submitter().register_buffers_sparse(n)
    }

    pub(crate) fn register_buffers_update(&self, off: u32, bufs: &[libc::iovec]) -> io::Result<()> {
        // SAFETY: the pool owns these pages for the life of the worker.
        unsafe {
            self.ring
                .submitter()
                .register_buffers_update(off, bufs, None)
        }?;
        Ok(())
    }

    pub(crate) fn as_raw_fd(&self) -> RawFd {
        self.ring.as_raw_fd()
    }
}
