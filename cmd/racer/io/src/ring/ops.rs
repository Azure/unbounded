//! Operations and their named future types.
//!
//! Each operation moves its owners (descriptor, buffers, lease, paths) into a
//! reactor slot on first poll and returns them only after every fence.
use super::{
    Completion, Descriptor, IoBuffer, IoFuture, OpCtx, Reactor, SendBuffer, SharedWriteBuffer,
    SocketAddress, VectoredCompletion, encode_address,
    op::{Op, Scratch},
    sealed::OpOwner,
};
use crate::{Error, Result};
use io_uring::types;
use std::{
    ffi::CString,
    os::fd::{FromRawFd, OwnedFd, RawFd},
    rc::Rc,
};

/// Positioned read future.
pub type Read<B, L = ()> = IoFuture<owners::ReadAt<B, L>>;
/// Positioned write future.
pub type Write<B, L = ()> = IoFuture<owners::WriteAt<B, L>>;
/// Positioned two-segment read future.
pub type ReadVectored<H, B, L = ()> = IoFuture<owners::ReadVectoredAt<H, B, L>>;
/// Positioned two-segment write future.
pub type WriteVectored<H, B, L = ()> = IoFuture<owners::WriteVectoredAt<H, B, L>>;
/// Socket receive future.
pub type Recv<B, L = ()> = IoFuture<owners::RecvOp<B, L>>;
/// Socket send future.
pub type Send<B, L = ()> = IoFuture<owners::SendOp<B, L>>;
/// `recvmsg` future with descriptors and kernel TLS record type.
pub type RecvMsg<B, L = ()> = IoFuture<owners::RecvMsgOp<B, L>>;
/// `sendmsg` future carrying `SCM_RIGHTS` descriptors.
pub type SendMsg<B, L = ()> = IoFuture<owners::SendMsgOp<B, L>>;
/// Accept future.
pub type Accept = IoFuture<owners::AcceptOp>;
/// Connect future.
pub type Connect<L = ()> = IoFuture<owners::ConnectOp<L>>;
/// Readiness future.
pub type PollReady<L = ()> = IoFuture<owners::PollOp<L>>;
/// Asynchronous close future.
pub type Close = IoFuture<owners::CloseOp>;
/// `fsync`/`fdatasync` future.
pub type Fsync = IoFuture<owners::FsyncOp>;
/// `openat2` future.
pub type Open = IoFuture<owners::OpenOp>;
/// `statx` future.
pub type Stat = IoFuture<owners::StatOp>;
/// `renameat2` future.
pub type Rename = IoFuture<owners::RenameOp>;
/// `unlinkat` future.
pub type Unlink = IoFuture<owners::UnlinkOp>;
/// `mkdirat` future.
pub type Mkdir = IoFuture<owners::MkdirOp>;
/// `splice` future.
pub type Splice = IoFuture<owners::SpliceOp>;

/// Most `SCM_RIGHTS` descriptors one receive accepts; more set `truncated`.
pub const MAX_RECEIVED_DESCRIPTORS: usize = 8;

/// Most descriptors one send can carry.
pub const MAX_SENT_DESCRIPTORS: usize = 8;

/// Linux `SOL_TLS` socket level.
pub const SOL_TLS: i32 = 282;

/// Linux `TLS_GET_RECORD_TYPE` control message type at [`SOL_TLS`].
pub const TLS_GET_RECORD_TYPE: i32 = 2;

/// Linux's pathname limit excluding NUL.
pub const PATH_BYTES: usize = 4095;

/// Readiness bits accepted by [`Reactor::poll_ready`].
pub const POLL_MASK: u32 =
    (libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP | libc::POLLHUP | libc::POLLERR) as u32;

/// A fenced `recvmsg` result.
#[derive(Debug)]
pub struct MessageCompletion<B, L = ()> {
    /// Original allocation.
    pub buffer: B,

    /// Bytes received.
    pub bytes: usize,

    /// Caller-owned guard retained through completion.
    pub lease: L,

    /// Received `SCM_RIGHTS` descriptors, close-on-exec.
    pub descriptors: Vec<OwnedFd>,

    /// The kernel reported `MSG_CTRUNC`.
    pub truncated: bool,

    /// Kernel TLS record content type, on kernel TLS sockets only.
    pub tls_record_type: Option<u8>,
}

/// Map a raw result to a byte count or error.
fn value(result: i32) -> Result<i32> {
    if result >= 0 {
        Ok(result)
    } else {
        Err(Error::from_errno(-result))
    }
}

/// Validate an explicit file offset.
fn offset_ok(offset: u64) -> Result<u64> {
    if offset > i64::MAX as u64 {
        return Err(Error::InvalidInput);
    }
    Ok(offset)
}

/// Convert a slice length to an SQE length.
fn len32(len: usize) -> Result<u32> {
    u32::try_from(len).map_err(|_| Error::InvalidInput)
}

/// Validate and NUL-terminate a pathname.
fn path_bytes(path: CString) -> Result<Vec<u8>> {
    if path.as_bytes().len() > PATH_BYTES {
        return Err(Error::InvalidInput);
    }
    Ok(path.into_bytes_with_nul())
}

/// Validate `openat2` flags and choose owner-only creation permissions.
pub(crate) fn open_flags(flags: i32) -> Result<(i32, u64)> {
    if flags & libc::O_PATH != 0 {
        if flags & !(libc::O_PATH | libc::O_CLOEXEC | libc::O_DIRECTORY | libc::O_NOFOLLOW) != 0 {
            return Err(Error::InvalidInput);
        }
        return Ok((flags | libc::O_CLOEXEC, 0));
    }
    let tmpfile = flags & libc::O_TMPFILE == libc::O_TMPFILE;
    if (tmpfile && flags & libc::O_ACCMODE == libc::O_RDONLY)
        || flags & libc::O_ACCMODE == libc::O_ACCMODE
    {
        return Err(Error::InvalidInput);
    }
    let mode = if flags & libc::O_CREAT != 0 || tmpfile {
        0o600
    } else {
        0
    };
    Ok((flags | libc::O_CLOEXEC | libc::O_NONBLOCK, mode))
}

pub mod owners {
    //! Owner types behind the named futures. Their fields are private; they
    //! exist only to name the futures.
    use super::*;

    /// Owners of [`super::Read`].
    pub struct ReadAt<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
        pub(super) offset: u64,
    }

    impl<B: IoBuffer, L: 'static> OpOwner for ReadAt<B, L> {
        type Value = Completion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLIN as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let bytes = self.buffer.bytes_mut()?;
            Ok(Op::Read {
                fd: self.fd.raw(),
                ptr: bytes.as_mut_ptr(),
                len: len32(bytes.len())?,
                offset: offset_ok(self.offset)?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(Completion {
                buffer: self.buffer,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::Write`].
    pub struct WriteAt<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
        pub(super) offset: u64,
    }

    impl<B: SharedWriteBuffer, L: 'static> OpOwner for WriteAt<B, L> {
        type Value = Completion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLOUT as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let bytes = self.buffer.write_bytes()?;
            Ok(Op::Write {
                fd: self.fd.raw(),
                ptr: bytes.as_ptr(),
                len: len32(bytes.len())?,
                offset: offset_ok(self.offset)?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(Completion {
                buffer: self.buffer,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::ReadVectored`].
    pub struct ReadVectoredAt<H, B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) header: H,
        pub(super) body: B,
        pub(super) lease: L,
        pub(super) offset: u64,
    }

    impl<H: IoBuffer, B: IoBuffer, L: 'static> OpOwner for ReadVectoredAt<H, B, L> {
        type Value = VectoredCompletion<H, B, L>;

        const RETRY_INTEREST: u32 = libc::POLLIN as u32;

        fn prepare(&mut self, scratch: &mut Scratch) -> Result<Op> {
            let header = self.header.bytes_mut()?;
            let (hp, hl) = (header.as_mut_ptr(), header.len());
            let body = self.body.bytes_mut()?;
            len32(hl.checked_add(body.len()).ok_or(Error::InvalidInput)?)?;
            scratch.iov = [
                libc::iovec {
                    iov_base: hp.cast(),
                    iov_len: hl,
                },
                libc::iovec {
                    iov_base: body.as_mut_ptr().cast(),
                    iov_len: body.len(),
                },
            ];
            Ok(Op::Readv {
                fd: self.fd.raw(),
                iov: scratch.iov.as_ptr(),
                count: 2,
                offset: offset_ok(self.offset)?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(VectoredCompletion {
                header: self.header,
                body: self.body,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::WriteVectored`].
    pub struct WriteVectoredAt<H, B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) header: H,
        pub(super) body: B,
        pub(super) lease: L,
        pub(super) offset: u64,
    }

    impl<H: SharedWriteBuffer, B: SharedWriteBuffer, L: 'static> OpOwner for WriteVectoredAt<H, B, L> {
        type Value = VectoredCompletion<H, B, L>;

        const RETRY_INTEREST: u32 = libc::POLLOUT as u32;

        fn prepare(&mut self, scratch: &mut Scratch) -> Result<Op> {
            let header = self.header.write_bytes()?;
            let body = self.body.write_bytes()?;
            len32(
                header
                    .len()
                    .checked_add(body.len())
                    .ok_or(Error::InvalidInput)?,
            )?;
            scratch.iov = [
                libc::iovec {
                    iov_base: header.as_ptr().cast_mut().cast(),
                    iov_len: header.len(),
                },
                libc::iovec {
                    iov_base: body.as_ptr().cast_mut().cast(),
                    iov_len: body.len(),
                },
            ];
            Ok(Op::Writev {
                fd: self.fd.raw(),
                iov: scratch.iov.as_ptr(),
                count: 2,
                offset: offset_ok(self.offset)?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(VectoredCompletion {
                header: self.header,
                body: self.body,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::Recv`].
    pub struct RecvOp<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
    }

    impl<B: IoBuffer, L: 'static> OpOwner for RecvOp<B, L> {
        type Value = Completion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLIN as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let bytes = self.buffer.bytes_mut()?;
            Ok(Op::Recv {
                fd: self.fd.raw(),
                ptr: bytes.as_mut_ptr(),
                len: len32(bytes.len())?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(Completion {
                buffer: self.buffer,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::Send`].
    pub struct SendOp<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
    }

    impl<B: SendBuffer, L: 'static> OpOwner for SendOp<B, L> {
        type Value = Completion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLOUT as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let bytes = self.buffer.send_bytes()?;
            Ok(Op::Send {
                fd: self.fd.raw(),
                ptr: bytes.as_ptr(),
                len: len32(bytes.len())?,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(Completion {
                buffer: self.buffer,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Control words able to hold the received descriptors.
    const CONTROL_WORDS: usize = 8;

    /// Owners of [`super::RecvMsg`]: the header, iovec, and control buffer
    /// live in the owner, which never moves while the receive is in flight.
    pub struct RecvMsgOp<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
        pub(super) msg: libc::msghdr,
        pub(super) iov: libc::iovec,
        pub(super) control: [u64; CONTROL_WORDS],
        pub(super) sim: bool,
    }

    impl<B, L> RecvMsgOp<B, L> {
        /// Control bytes able to hold [`MAX_RECEIVED_DESCRIPTORS`] descriptors.
        fn control_bytes() -> usize {
            // SAFETY: CMSG_SPACE only computes an aligned size.
            let bytes = unsafe {
                libc::CMSG_SPACE((MAX_RECEIVED_DESCRIPTORS * std::mem::size_of::<i32>()) as u32)
            } as usize;
            debug_assert!(bytes <= CONTROL_WORDS * 8);
            bytes
        }

        /// Walk control messages, calling `f(level, type, data)`.
        // Control lengths are usize on glibc but narrower on other libcs.
        #[allow(clippy::unnecessary_cast)]
        fn each_cmsg(&self, mut f: impl FnMut(i32, i32, *const u8, usize)) {
            if self.msg.msg_control.is_null() {
                return;
            }
            let used = (self.msg.msg_controllen as usize).min(CONTROL_WORDS * 8);
            let base = self.control.as_ptr().cast::<u8>();
            // SAFETY: CMSG_LEN(0) is the aligned header size.
            let data = unsafe { libc::CMSG_LEN(0) } as usize;
            let mut offset = 0;
            while offset + std::mem::size_of::<libc::cmsghdr>() <= used {
                // SAFETY: the header lies inside the initialized control buffer.
                let cmsg: libc::cmsghdr =
                    unsafe { std::ptr::read_unaligned(base.add(offset).cast()) };
                let length = cmsg.cmsg_len as usize;
                if length < std::mem::size_of::<libc::cmsghdr>() || offset + length > used {
                    break;
                }
                // SAFETY: the payload lies inside this control message.
                f(
                    cmsg.cmsg_level,
                    cmsg.cmsg_type,
                    unsafe { base.add(offset + data) },
                    length.saturating_sub(data),
                );
                offset += length.next_multiple_of(std::mem::size_of::<usize>());
            }
        }

        /// Take every `SCM_RIGHTS` descriptor, clearing the control buffer.
        fn take_descriptors(&mut self) -> Vec<OwnedFd> {
            let mut fds = Vec::new();
            if self.sim {
                return fds;
            }
            self.each_cmsg(|level, kind, data, len| {
                if level == libc::SOL_SOCKET && kind == libc::SCM_RIGHTS {
                    for index in 0..len / std::mem::size_of::<i32>() {
                        // SAFETY: the descriptor array is inside the message.
                        let fd: i32 =
                            unsafe { std::ptr::read_unaligned(data.add(index * 4).cast()) };
                        if fd >= 0 {
                            // SAFETY: the kernel installed this descriptor for us alone.
                            fds.push(unsafe { OwnedFd::from_raw_fd(fd) });
                        }
                    }
                }
            });
            self.control = [0; CONTROL_WORDS];
            self.msg.msg_controllen = 0;
            fds
        }

        /// Kernel TLS record type in the control buffer, if any.
        fn tls_record_type(&self) -> Option<u8> {
            let mut found = None;
            self.each_cmsg(|level, kind, data, len| {
                if found.is_none() && level == SOL_TLS && kind == TLS_GET_RECORD_TYPE && len > 0 {
                    // SAFETY: the one-byte payload is inside this message.
                    found = Some(unsafe { *data });
                }
            });
            found
        }
    }

    impl<B: IoBuffer, L: 'static> OpOwner for RecvMsgOp<B, L> {
        type Value = MessageCompletion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLIN as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            // Close anything left by an earlier attempt before reuse.
            drop(self.take_descriptors());
            let bytes = self.buffer.bytes_mut()?;
            self.iov = libc::iovec {
                iov_base: bytes.as_mut_ptr().cast(),
                iov_len: len32(bytes.len())? as usize,
            };
            // SAFETY: msghdr is plain data; zero is a valid empty value.
            self.msg = unsafe { std::mem::zeroed() };
            self.msg.msg_iov = &mut self.iov;
            self.msg.msg_iovlen = 1;
            self.msg.msg_control = self.control.as_mut_ptr().cast();
            self.msg.msg_controllen = Self::control_bytes() as _;
            Ok(Op::RecvMsg {
                fd: self.fd.raw(),
                msg: &mut self.msg,
            })
        }

        fn finish(mut self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            let truncated = !self.sim && self.msg.msg_flags & libc::MSG_CTRUNC != 0;
            let tls_record_type = if self.sim {
                None
            } else {
                self.tls_record_type()
            };
            let descriptors = self.take_descriptors();
            let Self { buffer, lease, .. } = self;
            Ok(MessageCompletion {
                buffer,
                bytes,
                lease,
                descriptors,
                truncated,
                tls_record_type,
            })
        }

        fn discard(mut self, _: i32) {
            drop(self.take_descriptors());
        }
    }

    /// Owners of [`super::SendMsg`].
    pub struct SendMsgOp<B, L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) buffer: B,
        pub(super) lease: L,
        pub(super) fds: Vec<Rc<Descriptor>>,
        pub(super) msg: libc::msghdr,
        pub(super) iov: libc::iovec,
        pub(super) control: [u64; CONTROL_WORDS],
    }

    impl<B: SendBuffer, L: 'static> OpOwner for SendMsgOp<B, L> {
        type Value = Completion<B, L>;

        const RETRY_INTEREST: u32 = libc::POLLOUT as u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            if self.fds.len() > MAX_SENT_DESCRIPTORS {
                return Err(Error::InvalidInput);
            }
            let bytes = self.buffer.send_bytes()?;
            self.iov = libc::iovec {
                iov_base: bytes.as_ptr().cast_mut().cast(),
                iov_len: len32(bytes.len())? as usize,
            };
            // SAFETY: msghdr is plain data; zero is a valid empty value.
            self.msg = unsafe { std::mem::zeroed() };
            self.msg.msg_iov = &mut self.iov;
            self.msg.msg_iovlen = 1;
            if !self.fds.is_empty() {
                let payload = (self.fds.len() * 4) as u32;
                self.control = [0; CONTROL_WORDS];
                let base = self.control.as_mut_ptr().cast::<u8>();
                // SAFETY: CMSG_LEN/CMSG_SPACE only compute sizes.
                let (len, space, data) = unsafe {
                    (
                        libc::CMSG_LEN(payload) as usize,
                        libc::CMSG_SPACE(payload) as usize,
                        libc::CMSG_LEN(0) as usize,
                    )
                };
                // SAFETY: zeroed cmsghdr is valid plain data.
                let mut header: libc::cmsghdr = unsafe { std::mem::zeroed() };
                header.cmsg_len = len as _;
                header.cmsg_level = libc::SOL_SOCKET;
                header.cmsg_type = libc::SCM_RIGHTS;
                // SAFETY: the control buffer holds CMSG_SPACE(8 * 4) bytes.
                unsafe {
                    std::ptr::write_unaligned(base.cast(), header);
                    for (index, fd) in self.fds.iter().enumerate() {
                        let raw = fd.host().ok_or(Error::InvalidInput)?;
                        std::ptr::write_unaligned(
                            base.add(data + index * 4).cast::<i32>(),
                            std::os::fd::AsRawFd::as_raw_fd(&raw),
                        );
                    }
                }
                self.msg.msg_control = base.cast();
                self.msg.msg_controllen = space as _;
            }
            Ok(Op::SendMsg {
                fd: self.fd.raw(),
                msg: &self.msg,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            let bytes = value(result)? as usize;
            Ok(Completion {
                buffer: self.buffer,
                bytes,
                lease: self.lease,
            })
        }
    }

    /// Owners of [`super::Accept`].
    pub struct AcceptOp {
        pub(super) fd: Rc<Descriptor>,
    }

    impl OpOwner for AcceptOp {
        type Value = Descriptor;

        const RETURNS_FD: bool = true;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::Accept { fd: self.fd.raw() })
        }

        fn finish(self, result: i32, reactor: &Reactor) -> Result<Self::Value> {
            Ok(reactor.adopt(value(result)?))
        }
    }

    /// Owners of [`super::Connect`].
    pub struct ConnectOp<L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) address: SocketAddress,
        pub(super) lease: L,
    }

    impl<L: 'static> OpOwner for ConnectOp<L> {
        type Value = L;

        fn prepare(&mut self, scratch: &mut Scratch) -> Result<Op> {
            let len = encode_address(&self.address, &mut scratch.addr)?;
            Ok(Op::Connect {
                fd: self.fd.raw(),
                addr: (&scratch.addr as *const libc::sockaddr_storage).cast(),
                len,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            value(result)?;
            Ok(self.lease)
        }
    }

    /// Owners of [`super::PollReady`].
    pub struct PollOp<L> {
        pub(super) fd: Rc<Descriptor>,
        pub(super) mask: u32,
        /// Retained until every fence, then dropped.
        #[allow(dead_code)]
        pub(super) lease: L,
    }

    impl<L: 'static> OpOwner for PollOp<L> {
        type Value = u32;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            if self.mask == 0 || self.mask & !POLL_MASK != 0 {
                return Err(Error::InvalidInput);
            }
            Ok(Op::Poll {
                fd: self.fd.raw(),
                mask: self.mask,
                multi: false,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            Ok(value(result)? as u32)
        }
    }

    /// Owners of [`super::Close`].
    pub struct CloseOp {
        pub(super) fd: Option<Descriptor>,
    }

    impl OpOwner for CloseOp {
        type Value = ();

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let fd = self.fd.as_ref().ok_or(Error::InvalidInput)?;
            Ok(Op::Close { fd: fd.raw() })
        }

        fn finish(mut self, result: i32, _: &Reactor) -> Result<Self::Value> {
            // The kernel consumed the descriptor whatever close returned,
            // except when the request was canceled before running.
            if result != -libc::ECANCELED
                && let Some(fd) = self.fd.take()
            {
                fd.into_raw();
            }
            value(result).map(|_| ())
        }

        fn discard(mut self, result: i32) {
            if result != -libc::ECANCELED
                && let Some(fd) = self.fd.take()
            {
                fd.into_raw();
            }
        }
    }

    /// Owners of [`super::Fsync`].
    pub struct FsyncOp {
        pub(super) fd: Rc<Descriptor>,
        pub(super) datasync: bool,
    }

    impl OpOwner for FsyncOp {
        type Value = ();

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::Fsync {
                fd: self.fd.raw(),
                datasync: self.datasync,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            value(result).map(|_| ())
        }
    }

    /// Owners of [`super::Open`].
    pub struct OpenOp {
        pub(super) dir: Option<Rc<Descriptor>>,
        pub(super) path: Vec<u8>,
        pub(super) flags: i32,
        pub(super) mode: u64,
        pub(super) resolve: u64,
    }

    impl OpOwner for OpenOp {
        type Value = Descriptor;

        const RETURNS_FD: bool = true;

        fn prepare(&mut self, scratch: &mut Scratch) -> Result<Op> {
            scratch.how = types::OpenHow::new()
                .flags(self.flags as u64)
                .mode(self.mode)
                .resolve(self.resolve);
            Ok(Op::OpenAt2 {
                dir: self.dir.as_ref().map_or(libc::AT_FDCWD, |dir| dir.raw()),
                path: self.path.as_ptr().cast(),
                how: &scratch.how,
            })
        }

        fn finish(self, result: i32, reactor: &Reactor) -> Result<Self::Value> {
            Ok(reactor.adopt(value(result)?))
        }
    }

    /// Owners of [`super::Stat`].
    pub struct StatOp {
        pub(super) fd: Rc<Descriptor>,
        pub(super) out: libc::statx,
    }

    impl OpOwner for StatOp {
        type Value = libc::statx;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::Statx {
                fd: self.fd.raw(),
                out: &mut self.out,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            value(result)?;
            Ok(self.out)
        }
    }

    /// Owners of [`super::Rename`].
    pub struct RenameOp {
        pub(super) dir: Rc<Descriptor>,
        pub(super) from: Vec<u8>,
        pub(super) to: Vec<u8>,
        pub(super) flags: u32,
    }

    impl OpOwner for RenameOp {
        type Value = ();

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::RenameAt {
                dir: self.dir.raw(),
                from: self.from.as_ptr().cast(),
                to: self.to.as_ptr().cast(),
                flags: self.flags,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            value(result).map(|_| ())
        }
    }

    /// Owners of [`super::Unlink`].
    pub struct UnlinkOp {
        pub(super) dir: Rc<Descriptor>,
        pub(super) path: Vec<u8>,
        pub(super) flags: i32,
        pub(super) missing_ok: bool,
    }

    impl OpOwner for UnlinkOp {
        type Value = ();

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::UnlinkAt {
                dir: self.dir.raw(),
                path: self.path.as_ptr().cast(),
                flags: self.flags,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            if self.missing_ok && result == -libc::ENOENT {
                return Ok(());
            }
            value(result).map(|_| ())
        }
    }

    /// Owners of [`super::Mkdir`].
    pub struct MkdirOp {
        pub(super) dir: Rc<Descriptor>,
        pub(super) path: Vec<u8>,
        pub(super) mode: u32,
        pub(super) exists_ok: bool,
    }

    impl OpOwner for MkdirOp {
        type Value = ();

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            Ok(Op::MkdirAt {
                dir: self.dir.raw(),
                path: self.path.as_ptr().cast(),
                mode: self.mode,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            if self.exists_ok && result == -libc::EEXIST {
                return Ok(());
            }
            value(result).map(|_| ())
        }
    }

    /// Owners of [`super::Splice`].
    pub struct SpliceOp {
        pub(super) input: Rc<Descriptor>,
        pub(super) input_offset: Option<u64>,
        pub(super) output: Rc<Descriptor>,
        pub(super) output_offset: Option<u64>,
        pub(super) len: u32,
    }

    impl OpOwner for SpliceOp {
        type Value = usize;

        fn prepare(&mut self, _: &mut Scratch) -> Result<Op> {
            let offset = |value: Option<u64>| -> Result<i64> {
                value.map_or(Ok(-1), |v| offset_ok(v).map(|v| v as i64))
            };
            Ok(Op::Splice {
                fd_in: self.input.raw(),
                off_in: offset(self.input_offset)?,
                fd_out: self.output.raw(),
                off_out: offset(self.output_offset)?,
                len: self.len,
                flags: libc::SPLICE_F_MOVE,
            })
        }

        fn finish(self, result: i32, _: &Reactor) -> Result<Self::Value> {
            Ok(value(result)? as usize)
        }
    }
}

use owners::*;

impl Reactor {
    /// Read into `buffer` at `offset`, retaining `lease` through every fence.
    pub fn read_at<B: IoBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        ctx: &OpCtx,
    ) -> Read<B, L> {
        IoFuture::new(
            self,
            ReadAt {
                fd,
                buffer,
                lease,
                offset,
            },
            ctx,
        )
    }

    /// Write `buffer` at `offset`; short progress is returned, never replayed.
    pub fn write_at<B: SharedWriteBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        ctx: &OpCtx,
    ) -> Write<B, L> {
        IoFuture::new(
            self,
            WriteAt {
                fd,
                buffer,
                lease,
                offset,
            },
            ctx,
        )
    }

    /// Read into `header` then `body` with one positioned `readv`.
    pub fn read_vectored_at<H: IoBuffer, B: IoBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        offset: u64,
        header: H,
        body: B,
        lease: L,
        ctx: &OpCtx,
    ) -> ReadVectored<H, B, L> {
        IoFuture::new(
            self,
            ReadVectoredAt {
                fd,
                header,
                body,
                lease,
                offset,
            },
            ctx,
        )
    }

    /// Write `header` then `body` contiguously with one positioned `writev`,
    /// without copying either. Short progress may end inside either segment.
    pub fn write_vectored_at<H: SharedWriteBuffer, B: SharedWriteBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        offset: u64,
        header: H,
        body: B,
        lease: L,
        ctx: &OpCtx,
    ) -> WriteVectored<H, B, L> {
        IoFuture::new(
            self,
            WriteVectoredAt {
                fd,
                header,
                body,
                lease,
                offset,
            },
            ctx,
        )
    }

    /// Receive into owned storage.
    pub fn recv<B: IoBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        ctx: &OpCtx,
    ) -> Recv<B, L> {
        IoFuture::new(self, RecvOp { fd, buffer, lease }, ctx)
    }

    /// Send owned immutable storage, suppressing `SIGPIPE`.
    pub fn send<B: SendBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        ctx: &OpCtx,
    ) -> Send<B, L> {
        IoFuture::new(self, SendOp { fd, buffer, lease }, ctx)
    }

    /// Receive like [`Self::recv`], also accepting `SCM_RIGHTS` descriptors
    /// (close-on-exec) and reporting kernel TLS record types. A
    /// non-application TLS record is returned here instead of failing with
    /// `EIO`. Simulated sockets carry no ancillary data.
    pub fn recv_msg<B: IoBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        ctx: &OpCtx,
    ) -> RecvMsg<B, L> {
        let sim = fd.is_sim();
        IoFuture::new(
            self,
            RecvMsgOp {
                fd,
                buffer,
                lease,
                // SAFETY: msghdr is plain data; zero is a valid empty value.
                msg: unsafe { std::mem::zeroed() },
                iov: libc::iovec {
                    iov_base: std::ptr::null_mut(),
                    iov_len: 0,
                },
                control: [0; 8],
                sim,
            },
            ctx,
        )
    }

    /// Send bytes plus up to [`MAX_SENT_DESCRIPTORS`] host descriptors with
    /// `SCM_RIGHTS`. The descriptors stay owned by the caller's `Rc`s.
    pub fn send_msg<B: SendBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        buffer: B,
        fds: Vec<Rc<Descriptor>>,
        lease: L,
        ctx: &OpCtx,
    ) -> SendMsg<B, L> {
        IoFuture::new(
            self,
            SendMsgOp {
                fd,
                buffer,
                lease,
                fds,
                // SAFETY: msghdr is plain data; zero is a valid empty value.
                msg: unsafe { std::mem::zeroed() },
                iov: libc::iovec {
                    iov_base: std::ptr::null_mut(),
                    iov_len: 0,
                },
                control: [0; 8],
            },
            ctx,
        )
    }

    /// Accept one nonblocking, close-on-exec connection.
    pub fn accept(&self, fd: Rc<Descriptor>, ctx: &OpCtx) -> Accept {
        IoFuture::new(self, AcceptOp { fd }, ctx)
    }

    /// Connect once (never retried), returning `lease` after every fence.
    pub fn connect<L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        lease: L,
        ctx: &OpCtx,
    ) -> Connect<L> {
        IoFuture::new(self, ConnectOp { fd, address, lease }, ctx)
    }

    /// Wait for readiness in `mask` (a subset of [`POLL_MASK`]); returns revents.
    pub fn poll_ready<L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        mask: u32,
        lease: L,
        ctx: &OpCtx,
    ) -> PollReady<L> {
        IoFuture::new(self, PollOp { fd, mask, lease }, ctx)
    }

    /// Close a descriptor on the ring.
    pub fn close(&self, fd: Descriptor, ctx: &OpCtx) -> Close {
        IoFuture::new(self, CloseOp { fd: Some(fd) }, ctx)
    }

    /// `fsync` (or `fdatasync` when `datasync`).
    pub fn fsync(&self, fd: Rc<Descriptor>, datasync: bool, ctx: &OpCtx) -> Fsync {
        IoFuture::new(self, FsyncOp { fd, datasync }, ctx)
    }

    /// `openat2` relative to `dir` (or the working directory). Created files
    /// are owner-only (0600 before umask). Data descriptors are nonblocking.
    pub fn open(
        &self,
        dir: Option<Rc<Descriptor>>,
        path: CString,
        flags: i32,
        resolve: u64,
        ctx: &OpCtx,
    ) -> Result<Open> {
        let (flags, mode) = open_flags(flags)?;
        Ok(IoFuture::new(
            self,
            OpenOp {
                dir,
                path: path_bytes(path)?,
                flags,
                mode,
                resolve,
            },
            ctx,
        ))
    }

    /// `statx` of an open descriptor (basic stats).
    pub fn stat(&self, fd: Rc<Descriptor>, ctx: &OpCtx) -> Stat {
        IoFuture::new(
            self,
            StatOp {
                fd,
                // SAFETY: statx is plain data; zero is valid.
                out: unsafe { std::mem::zeroed() },
            },
            ctx,
        )
    }

    /// `renameat2` within one directory.
    pub fn rename(
        &self,
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
        flags: u32,
        ctx: &OpCtx,
    ) -> Result<Rename> {
        Ok(IoFuture::new(
            self,
            RenameOp {
                dir,
                from: path_bytes(from)?,
                to: path_bytes(to)?,
                flags,
            },
            ctx,
        ))
    }

    /// `unlinkat`; with `missing_ok` an absent entry is success.
    pub fn unlink(
        &self,
        dir: Rc<Descriptor>,
        path: CString,
        flags: i32,
        missing_ok: bool,
        ctx: &OpCtx,
    ) -> Result<Unlink> {
        Ok(IoFuture::new(
            self,
            UnlinkOp {
                dir,
                path: path_bytes(path)?,
                flags,
                missing_ok,
            },
            ctx,
        ))
    }

    /// `mkdirat`; with `exists_ok` an existing entry is success.
    pub fn mkdir(
        &self,
        dir: Rc<Descriptor>,
        path: CString,
        mode: u32,
        exists_ok: bool,
        ctx: &OpCtx,
    ) -> Result<Mkdir> {
        Ok(IoFuture::new(
            self,
            MkdirOp {
                dir,
                path: path_bytes(path)?,
                mode,
                exists_ok,
            },
            ctx,
        ))
    }

    /// Move up to `len` bytes between descriptors without copying through
    /// user space (one side must be a pipe). `None` offsets use file position.
    pub fn splice(
        &self,
        input: Rc<Descriptor>,
        input_offset: Option<u64>,
        output: Rc<Descriptor>,
        output_offset: Option<u64>,
        len: u32,
        ctx: &OpCtx,
    ) -> Splice {
        IoFuture::new(
            self,
            SpliceOp {
                input,
                input_offset,
                output,
                output_offset,
                len,
            },
            ctx,
        )
    }

    /// Wait until a host descriptor (for example a [`crate::signal::SignalFd`])
    /// is readable, without taking ownership. The caller must keep `fd` open
    /// until the returned future and its fences complete; prefer
    /// [`Self::poll_ready`] with an owned descriptor where possible.
    ///
    /// # Safety
    /// `fd` must stay open until the operation is fenced.
    pub unsafe fn readable_raw(&self, fd: RawFd, ctx: &OpCtx) -> PollReady<()> {
        let owner = PollOp {
            // SAFETY: the caller keeps the descriptor open until the fence.
            fd: Rc::new(unsafe { Descriptor::borrowed(fd) }),
            mask: libc::POLLIN as u32,
            lease: (),
        };
        IoFuture::new(self, owner, ctx)
    }
}
