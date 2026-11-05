//! Backend-neutral operation descriptions and their kernel lowering.
//!
//! Every pointer in an [`Op`] references memory owned by the operation's slot
//! (its owner cell or scratch), which outlives the original and any cancel CQE.
use io_uring::{opcode, squeue, types};
use std::os::fd::RawFd;

/// One submission, independent of the backend that executes it.
#[derive(Clone, Copy, Debug)]
pub enum Op {
    Read {
        fd: RawFd,
        ptr: *mut u8,
        len: u32,
        offset: u64,
    },
    Write {
        fd: RawFd,
        ptr: *const u8,
        len: u32,
        offset: u64,
    },
    Readv {
        fd: RawFd,
        iov: *const libc::iovec,
        count: u32,
        offset: u64,
    },
    Writev {
        fd: RawFd,
        iov: *const libc::iovec,
        count: u32,
        offset: u64,
    },
    Recv {
        fd: RawFd,
        ptr: *mut u8,
        len: u32,
    },
    Send {
        fd: RawFd,
        ptr: *const u8,
        len: u32,
    },
    RecvMsg {
        fd: RawFd,
        msg: *mut libc::msghdr,
    },
    SendMsg {
        fd: RawFd,
        msg: *const libc::msghdr,
    },
    Accept {
        fd: RawFd,
    },
    Connect {
        fd: RawFd,
        addr: *const libc::sockaddr,
        len: libc::socklen_t,
    },
    Poll {
        fd: RawFd,
        mask: u32,
        multi: bool,
    },
    Close {
        fd: RawFd,
    },
    Fsync {
        fd: RawFd,
        datasync: bool,
    },
    OpenAt2 {
        dir: RawFd,
        path: *const libc::c_char,
        how: *const types::OpenHow,
    },
    Statx {
        fd: RawFd,
        out: *mut libc::statx,
    },
    RenameAt {
        dir: RawFd,
        from: *const libc::c_char,
        to: *const libc::c_char,
        flags: u32,
    },
    UnlinkAt {
        dir: RawFd,
        path: *const libc::c_char,
        flags: i32,
    },
    MkdirAt {
        dir: RawFd,
        path: *const libc::c_char,
        mode: u32,
    },
    Splice {
        fd_in: RawFd,
        off_in: i64,
        fd_out: RawFd,
        off_out: i64,
        len: u32,
        flags: u32,
    },
    Cancel {
        target: u64,
    },
}

impl Op {
    /// Short stable name used by the simulator for faults and traces.
    pub fn name(&self) -> &'static str {
        match self {
            Self::Read { .. } => "read",
            Self::Write { .. } => "write",
            Self::Readv { .. } => "readv",
            Self::Writev { .. } => "writev",
            Self::Recv { .. } => "recv",
            Self::Send { .. } => "send",
            Self::RecvMsg { .. } => "recvmsg",
            Self::SendMsg { .. } => "sendmsg",
            Self::Accept { .. } => "accept",
            Self::Connect { .. } => "connect",
            Self::Poll { .. } => "poll",
            Self::Close { .. } => "close",
            Self::Fsync { .. } => "fsync",
            Self::OpenAt2 { .. } => "open",
            Self::Statx { .. } => "stat",
            Self::RenameAt { .. } => "rename",
            Self::UnlinkAt { .. } => "unlink",
            Self::MkdirAt { .. } => "mkdir",
            Self::Splice { .. } => "splice",
            Self::Cancel { .. } => "cancel",
        }
    }

    /// Lower to a kernel SQE, adding `IOSQE_ASYNC` to positioned file reads
    /// and writes when `offload` is set. Returns whether the SQE is offloaded.
    pub fn issue(&self, offload: bool) -> (squeue::Entry, bool) {
        let sqe = self.sqe();
        let positioned = matches!(
            self,
            Self::Read { .. } | Self::Write { .. } | Self::Readv { .. } | Self::Writev { .. }
        );
        if offload && positioned {
            return (sqe.flags(squeue::Flags::ASYNC), true);
        }
        let metadata = matches!(
            self,
            Self::Fsync { .. }
                | Self::OpenAt2 { .. }
                | Self::Statx { .. }
                | Self::RenameAt { .. }
                | Self::UnlinkAt { .. }
                | Self::MkdirAt { .. }
        );
        (sqe, metadata)
    }

    /// Lower to a kernel SQE. Filesystem metadata ops may block, so they are
    /// issued with `IOSQE_ASYNC`; data I/O is always issued inline.
    pub fn sqe(&self) -> squeue::Entry {
        let fd = types::Fd;
        let async_ = squeue::Flags::ASYNC;
        match *self {
            Self::Read {
                fd: f,
                ptr,
                len,
                offset,
            } => opcode::Read::new(fd(f), ptr, len).offset(offset).build(),
            Self::Write {
                fd: f,
                ptr,
                len,
                offset,
            } => opcode::Write::new(fd(f), ptr, len).offset(offset).build(),
            Self::Readv {
                fd: f,
                iov,
                count,
                offset,
            } => opcode::Readv::new(fd(f), iov, count).offset(offset).build(),
            Self::Writev {
                fd: f,
                iov,
                count,
                offset,
            } => opcode::Writev::new(fd(f), iov, count)
                .offset(offset)
                .build(),
            Self::Recv { fd: f, ptr, len } => opcode::Recv::new(fd(f), ptr, len).build(),
            Self::Send { fd: f, ptr, len } => opcode::Send::new(fd(f), ptr, len)
                .flags(libc::MSG_NOSIGNAL)
                .build(),
            Self::RecvMsg { fd: f, msg } => opcode::RecvMsg::new(fd(f), msg)
                .flags(libc::MSG_CMSG_CLOEXEC as u32)
                .build(),
            Self::SendMsg { fd: f, msg } => opcode::SendMsg::new(fd(f), msg)
                .flags(libc::MSG_NOSIGNAL as u32)
                .build(),
            Self::Accept { fd: f } => {
                opcode::Accept::new(fd(f), std::ptr::null_mut(), std::ptr::null_mut())
                    .flags(libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK)
                    .build()
            }
            Self::Connect { fd: f, addr, len } => opcode::Connect::new(fd(f), addr, len).build(),
            Self::Poll { fd: f, mask, multi } => {
                opcode::PollAdd::new(fd(f), mask).multi(multi).build()
            }
            Self::Close { fd: f } => opcode::Close::new(fd(f)).build(),
            Self::Fsync { fd: f, datasync } => opcode::Fsync::new(fd(f))
                .flags(if datasync {
                    types::FsyncFlags::DATASYNC
                } else {
                    types::FsyncFlags::empty()
                })
                .build()
                .flags(async_),
            Self::OpenAt2 { dir, path, how } => opcode::OpenAt2::new(fd(dir), path, how)
                .build()
                .flags(async_),
            Self::Statx { fd: f, out } => opcode::Statx::new(fd(f), c"".as_ptr(), out.cast())
                .flags(libc::AT_EMPTY_PATH)
                .mask(libc::STATX_BASIC_STATS)
                .build()
                .flags(async_),
            Self::RenameAt {
                dir,
                from,
                to,
                flags,
            } => opcode::RenameAt::new(fd(dir), from, fd(dir), to)
                .flags(flags)
                .build()
                .flags(async_),
            Self::UnlinkAt { dir, path, flags } => opcode::UnlinkAt::new(fd(dir), path)
                .flags(flags)
                .build()
                .flags(async_),
            Self::MkdirAt { dir, path, mode } => opcode::MkDirAt::new(fd(dir), path)
                .mode(mode)
                .build()
                .flags(async_),
            Self::Splice {
                fd_in,
                off_in,
                fd_out,
                off_out,
                len,
                flags,
            } => opcode::Splice::new(fd(fd_in), off_in, fd(fd_out), off_out, len)
                .flags(flags)
                .build(),
            Self::Cancel { target } => opcode::AsyncCancel::new(target).build(),
        }
    }
}

/// Per-slot syscall arguments with stable addresses, reused across operations
/// so building an operation never allocates.
pub struct Scratch {
    pub iov: [libc::iovec; 2],

    pub msg: libc::msghdr,

    pub addr: libc::sockaddr_storage,

    pub how: types::OpenHow,
}

impl Default for Scratch {
    /// Zeroed scratch space.
    fn default() -> Self {
        Self::new()
    }
}

impl Scratch {
    /// Zeroed scratch space.
    pub fn new() -> Self {
        let empty = libc::iovec {
            iov_base: std::ptr::null_mut(),
            iov_len: 0,
        };
        Self {
            iov: [empty; 2],
            // SAFETY: msghdr and sockaddr_storage are plain data; zero is valid.
            msg: unsafe { std::mem::zeroed() },
            // SAFETY: as above.
            addr: unsafe { std::mem::zeroed() },
            how: types::OpenHow::new(),
        }
    }
}
