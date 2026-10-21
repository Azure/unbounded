//! Typed operations and their futures.
//!
//! Each operation is described by a small value type (an [`OpSpec`]) that
//! owns every input the backend reads or writes. [`Operation`] is the single
//! future type that submits a spec on its first poll and resolves to the
//! spec's output once every completion has arrived.

use std::ffi::{CStr, CString};
use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::ptr::NonNull;
use std::task::{Context, Poll};
use std::time::Instant;

use crate::backend::{KernelTimespec, Op, OpKind, OpenHow, PushHints};
use crate::{
    Backend, BufResult, Error, ErrorKind, IoBuf, IoBufMut, IoBufs, Result, Scope, SockAddr,
    SocketAddress,
};

use super::Reactor;
use super::admission::Reservation;
use super::table::Node;

mod sealed {
    /// Prevents implementations of [`OpSpec`](super::OpSpec) outside the crate.
    pub trait Sealed {}
}

/// An operation description that [`Operation`] can submit.
///
/// Implemented by the types in this module; it cannot be implemented
/// outside the crate. Use the constructors on [`Reactor`].
pub trait OpSpec<B: Backend>: sealed::Sealed + Sized + 'static {
    /// What the operation resolves to, given the scope's error type `E`.
    type Output<E>;

    #[doc(hidden)]
    const KIND: OpKind;

    /// Validate inputs before admission.
    #[doc(hidden)]
    fn prepare(&mut self) -> Result<()> {
        Ok(())
    }

    /// Describe the operation. Called once, after `self` reached its final
    /// heap address, so every pointer in the returned [`Op`] points into
    /// memory that the node owns until the fence.
    #[doc(hidden)]
    fn op(&mut self) -> Op<'_, B::Fd>;

    /// Map a raw completion result before generic error handling.
    #[doc(hidden)]
    fn normalize(raw: i32) -> i32 {
        raw
    }

    /// Take ownership of resources carried by a successful original
    /// completion, as soon as it arrives.
    #[doc(hidden)]
    fn deliver(&mut self, _backend: &mut B, _result: i32) {}

    /// Produce the output, returning every input to the caller.
    #[doc(hidden)]
    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E>;
}

/// The heap node of a submitted operation: the scope, the optional lease
/// and the spec. See the [module docs](super) for the ownership rules.
pub(super) struct NodeImpl<O, S, L> {
    pub(super) scope: S,
    pub(super) lease: L,
    pub(super) data: O,
}

impl<B: Backend, O: OpSpec<B>, S: Scope, L: 'static> Node<B> for NodeImpl<O, S, L> {
    fn scope_failed(&self) -> bool {
        self.scope.check().is_err()
    }

    fn scope_any(&self) -> &dyn std::any::Any {
        &self.scope
    }

    fn deliver(&mut self, backend: &mut B, result: i32) {
        self.data.deliver(backend, result);
    }
}

/// A lease attached with [`Operation::lease`].
#[derive(Debug)]
pub struct Leased<L>(L);

/// The future of one reactor operation.
///
/// Created by the operation methods on [`Reactor`]. Nothing happens until
/// the first poll, which checks the scope, admits the operation and queues
/// it with the backend; it is submitted to the kernel by the next
/// [`Reactor::poll`] or [`Reactor::wait`].
///
/// The output is the spec's `Output<S::Error>` (see each constructor); with
/// a [lease](Operation::lease) it is `(output, lease)`.
///
/// Dropping a submitted operation requests its cancellation. Its inputs and
/// lease are released by the reactor only after every completion arrived.
#[must_use = "operations do nothing until polled"]
pub struct Operation<'r, B: Backend, O: OpSpec<B>, S: Scope, L = ()> {
    reactor: &'r Reactor<B>,
    stage: Stage<O, S, L>,
}

enum Stage<O, S, L> {
    Unsubmitted {
        data: O,
        scope: S,
        lease: L,
        reservation: Option<Reservation>,
        hints: PushHints,
    },
    Submitted {
        key: u64,
        node: NonNull<NodeImpl<O, S, L>>,
    },
    Finished,
}

// The future never hands out pinned references to its fields; the node is
// heap allocated and addressed through a raw pointer.
impl<B: Backend, O: OpSpec<B>, S: Scope, L> Unpin for Operation<'_, B, O, S, L> {}

impl<'r, B: Backend, O: OpSpec<B>, S: Scope, L: 'static> Operation<'r, B, O, S, L> {
    fn new(reactor: &'r Reactor<B>, data: O, scope: &S, lease: L) -> Self {
        Self {
            reactor,
            stage: Stage::Unsubmitted {
                data,
                scope: scope.clone(),
                lease,
                reservation: None,
                hints: PushHints::new(),
            },
        }
    }

    /// Admit this operation against `reservation` instead of the ordinary
    /// partition.
    ///
    /// # Panics
    ///
    /// Panics if the operation was already polled.
    pub fn reserved(mut self, reservation: &Reservation) -> Self {
        match &mut self.stage {
            Stage::Unsubmitted { reservation: r, .. } => *r = Some(reservation.clone()),
            _ => panic!("Operation::reserved called after the first poll"),
        }
        self
    }

    /// Ask the backend to execute this operation in a kernel worker rather
    /// than inline during submission (see [`PushHints::offload`]). Use it for
    /// operations expected to block, such as cold buffered file I/O. It does
    /// not change the operation's result.
    ///
    /// # Panics
    ///
    /// Panics if the operation was already polled.
    pub fn offload(mut self) -> Self {
        match &mut self.stage {
            Stage::Unsubmitted { hints, .. } => *hints = hints.with_offload(true),
            _ => panic!("Operation::offload called after the first poll"),
        }
        self
    }

    /// Whether the operation was submitted and has not resolved yet.
    pub fn is_submitted(&self) -> bool {
        matches!(self.stage, Stage::Submitted { .. })
    }

    fn poll_inner(&mut self, cx: &mut Context<'_>) -> Poll<(O::Output<S::Error>, L)> {
        match std::mem::replace(&mut self.stage, Stage::Finished) {
            Stage::Unsubmitted {
                data,
                scope,
                lease,
                reservation,
                hints,
            } => {
                let node = NodeImpl { scope, lease, data };
                match self
                    .reactor
                    .submit_node(node, reservation.as_ref(), hints, cx.waker())
                {
                    Ok((key, node)) => {
                        self.stage = Stage::Submitted { key, node };
                        Poll::Pending
                    }
                    Err((node, error)) => Poll::Ready((node.data.finish(Err(error)), node.lease)),
                }
            }
            Stage::Submitted { key, node } => match self.reactor.collect(key, cx.waker()) {
                None => {
                    self.stage = Stage::Submitted { key, node };
                    Poll::Pending
                }
                Some((raw, canceled)) => {
                    // SAFETY: `collect` returned the result, so the operation is
                    // fenced and its entry removed: the backend and the reactor
                    // no longer reference the node, and this future is its only
                    // owner. It was allocated as a `Box<NodeImpl<O, S, L>>`.
                    let node = *unsafe { Box::from_raw(node.as_ptr()) };
                    let result = resolve::<B, O, S>(
                        raw,
                        canceled,
                        &node.scope,
                        self.reactor.config.cancel_wins,
                    );
                    Poll::Ready((node.data.finish(result), node.lease))
                }
            },
            Stage::Finished => panic!("Operation polled after completion"),
        }
    }
}

impl<'r, B: Backend, O: OpSpec<B>, S: Scope> Operation<'r, B, O, S, ()> {
    /// Attach a caller-owned value that the reactor holds until the
    /// operation is fenced, and then returns with the output.
    ///
    /// Use a lease for state whose lifetime must cover the kernel's use of
    /// the operation even if the future is dropped, such as an admission
    /// permit or a connection slot: if the future is dropped, the lease is
    /// dropped only after every completion arrived.
    ///
    /// # Panics
    ///
    /// Panics if the operation was already polled.
    pub fn lease<L: 'static>(mut self, lease: L) -> Operation<'r, B, O, S, Leased<L>> {
        let Stage::Unsubmitted {
            data,
            scope,
            reservation,
            hints,
            ..
        } = std::mem::replace(&mut self.stage, Stage::Finished)
        else {
            panic!("Operation::lease called after the first poll");
        };
        Operation {
            reactor: self.reactor,
            stage: Stage::Unsubmitted {
                data,
                scope,
                lease: Leased(lease),
                reservation,
                hints,
            },
        }
    }
}

impl<B: Backend, O: OpSpec<B>, S: Scope> Future for Operation<'_, B, O, S, ()> {
    type Output = O::Output<S::Error>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.poll_inner(cx).map(|(output, ())| output)
    }
}

impl<B: Backend, O: OpSpec<B>, S: Scope, L: 'static> Future for Operation<'_, B, O, S, Leased<L>> {
    type Output = (O::Output<S::Error>, L);

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.poll_inner(cx)
            .map(|(output, Leased(lease))| (output, lease))
    }
}

impl<B: Backend, O: OpSpec<B>, S: Scope, L> Drop for Operation<'_, B, O, S, L> {
    fn drop(&mut self) {
        if let Stage::Submitted { key, node } = self.stage
            && self.reactor.abandon(key)
        {
            // SAFETY: `abandon` returned true, so the operation was fenced and
            // its entry removed; this future is the node's only owner.
            drop(unsafe { Box::from_raw(node.as_ptr()) });
        }
    }
}

impl<B: Backend, O: OpSpec<B>, S: Scope, L> fmt::Debug for Operation<'_, B, O, S, L> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let stage = match self.stage {
            Stage::Unsubmitted { .. } => "unsubmitted",
            Stage::Submitted { .. } => "submitted",
            Stage::Finished => "finished",
        };
        f.debug_struct("Operation")
            .field("kind", &O::KIND)
            .field("stage", &stage)
            .finish()
    }
}

/// Apply cancellation precedence to a raw completion result.
fn resolve<B: Backend, O: OpSpec<B>, S: Scope>(
    raw: i32,
    canceled: bool,
    scope: &S,
    cancel_wins: bool,
) -> Result<u32, S::Error> {
    let raw = O::normalize(raw);
    if canceled && (cancel_wins || raw < 0) {
        return Err(match scope.check() {
            Err(error) => error,
            Ok(()) => Error::new(ErrorKind::Canceled).into(),
        });
    }
    u32::try_from(raw).map_err(|_| Error::from_errno(raw).into())
}

fn clamp_len(len: usize) -> u32 {
    u32::try_from(len).unwrap_or(u32::MAX)
}

fn check_offset(offset: u64) -> Result<()> {
    if offset > i64::MAX as u64 {
        return Err(
            Error::new(ErrorKind::InvalidInput).with_context("file offset exceeds i64::MAX")
        );
    }
    Ok(())
}

macro_rules! sealed {
    (buffered: $($ty:ident),* $(,)?) => {
        $(impl<B: Backend, Buf> sealed::Sealed for $ty<B, Buf> {})*
    };
    ($($ty:ident),* $(,)?) => {
        $(impl<B: Backend> sealed::Sealed for $ty<B> {})*
    };
}

/// Positioned read; see [`Reactor::read_at`].
pub struct ReadAt<B: Backend, Buf> {
    fd: B::Fd,
    offset: u64,
    buf: Buf,
}

/// Positioned write; see [`Reactor::write_at`].
pub struct WriteAt<B: Backend, Buf> {
    fd: B::Fd,
    offset: u64,
    buf: Buf,
}

/// Positioned vectored write; see [`Reactor::writev_at`].
pub struct WritevAt<B: Backend, Bufs> {
    fd: B::Fd,
    offset: u64,
    bufs: Bufs,
    /// The kernel-visible segment array, built once in `prepare`. A `Vec`
    /// rather than a `Box` so that moving it keeps derived pointers valid.
    iovecs: Vec<libc::iovec>,
}

/// Socket receive; see [`Reactor::recv`].
pub struct SocketRecv<B: Backend, Buf> {
    fd: B::Fd,
    buf: Buf,
}

/// Socket send; see [`Reactor::send`].
pub struct SocketSend<B: Backend, Buf> {
    fd: B::Fd,
    buf: Buf,
}

sealed!(buffered: ReadAt, WriteAt, WritevAt, SocketRecv, SocketSend);

/// One-shot readiness poll; see [`Reactor::readiness`].
pub struct Readiness<B: Backend> {
    fd: B::Fd,
    events: u32,
}

/// Accept a connection; see [`Reactor::accept`].
pub struct Accept<B: Backend> {
    fd: B::Fd,
    peer: SockAddr,
    accepted: Option<B::Fd>,
}

/// Connect a socket; see [`Reactor::connect`].
pub struct Connect<B: Backend> {
    fd: B::Fd,
    address: Result<SockAddr>,
}

/// Open a file; see [`Reactor::open_at2`].
pub struct OpenAt2<B: Backend> {
    dir: Option<B::Fd>,
    path: CString,
    how: OpenHow,
    opened: Option<B::Fd>,
}

/// File status; see [`Reactor::statx`].
pub struct Statx<B: Backend> {
    dir: Option<B::Fd>,
    path: CString,
    flags: i32,
    mask: u32,
    // A Vec, not a Box or an inline value: its heap storage does not move or
    // get retagged when the spec moves, and the kernel writes into it.
    out: Vec<libc::statx>,
}

/// Flush a file; see [`Reactor::fsync`].
pub struct Fsync<B: Backend> {
    fd: B::Fd,
    datasync: bool,
}

/// Rename a path; see [`Reactor::rename_at`].
pub struct RenameAt<B: Backend> {
    old_dir: Option<B::Fd>,
    old_path: CString,
    new_dir: Option<B::Fd>,
    new_path: CString,
    flags: u32,
}

/// Remove a path; see [`Reactor::unlink_at`].
pub struct UnlinkAt<B: Backend> {
    dir: Option<B::Fd>,
    path: CString,
    flags: i32,
}

/// Create a directory; see [`Reactor::mkdir_at`].
pub struct MkDirAt<B: Backend> {
    dir: Option<B::Fd>,
    path: CString,
    mode: u32,
}

/// Sleep until a deadline; see [`Reactor::sleep_until`].
pub struct SleepUntil<B: Backend> {
    deadline: Instant,
    timespec: KernelTimespec,
    _backend: std::marker::PhantomData<fn() -> B>,
}

/// Do nothing; see [`Reactor::nop`].
pub struct Nop<B: Backend>(std::marker::PhantomData<fn() -> B>);

sealed!(
    Readiness, Accept, Connect, OpenAt2, Statx, Fsync, RenameAt, UnlinkAt, MkDirAt, SleepUntil,
    Nop,
);

impl<B: Backend, Buf: IoBufMut> OpSpec<B> for ReadAt<B, Buf> {
    type Output<E> = BufResult<usize, Buf, E>;
    const KIND: OpKind = OpKind::Read;

    fn prepare(&mut self) -> Result<()> {
        check_offset(self.offset)
    }

    fn op(&mut self) -> Op<'_, B::Fd> {
        let len = clamp_len(self.buf.bytes_total());
        Op::Read {
            fd: &self.fd,
            offset: self.offset,
            buf: self.buf.stable_mut_ptr(),
            len,
        }
    }

    fn finish<E: From<Error>>(mut self, result: Result<u32, E>) -> Self::Output<E> {
        let result = result.map(|n| {
            // SAFETY: the backend reported `n` bytes written from the start of
            // the buffer, and `n <= bytes_total`.
            unsafe { self.buf.set_init(n as usize) };
            n as usize
        });
        BufResult::new(result, self.buf)
    }
}

impl<B: Backend, Buf: IoBuf> OpSpec<B> for WriteAt<B, Buf> {
    type Output<E> = BufResult<usize, Buf, E>;
    const KIND: OpKind = OpKind::Write;

    fn prepare(&mut self) -> Result<()> {
        check_offset(self.offset)
    }

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Write {
            fd: &self.fd,
            offset: self.offset,
            buf: self.buf.stable_ptr(),
            len: clamp_len(self.buf.bytes_init()),
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        BufResult::new(result.map(|n| n as usize), self.buf)
    }
}

/// Maximum segments per vectored write (Linux `UIO_MAXIOV`).
const MAX_IOVECS: usize = 1024;

impl<B: Backend, Bufs: IoBufs> OpSpec<B> for WritevAt<B, Bufs> {
    type Output<E> = BufResult<usize, Bufs, E>;
    const KIND: OpKind = OpKind::Writev;

    fn prepare(&mut self) -> Result<()> {
        check_offset(self.offset)?;
        let count = self.bufs.segment_count();
        if count > MAX_IOVECS {
            return Err(Error::new(ErrorKind::InvalidInput)
                .with_context("vectored write exceeds UIO_MAXIOV segments"));
        }
        let mut total = 0usize;
        self.iovecs = (0..count)
            .map(|index| {
                let (base, len) = self.bufs.segment(index);
                total = total.saturating_add(len);
                libc::iovec {
                    iov_base: base.cast_mut().cast(),
                    iov_len: len,
                }
            })
            .collect();
        if total > i32::MAX as usize {
            return Err(Error::new(ErrorKind::InvalidInput)
                .with_context("vectored write exceeds i32::MAX bytes"));
        }
        Ok(())
    }

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Writev {
            fd: &self.fd,
            offset: self.offset,
            iovecs: self.iovecs.as_ptr(),
            // `prepare` bounds the count by MAX_IOVECS.
            count: clamp_len(self.iovecs.len()),
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        BufResult::new(result.map(|n| n as usize), self.bufs)
    }
}

impl<B: Backend, Buf: IoBufMut> OpSpec<B> for SocketRecv<B, Buf> {
    type Output<E> = BufResult<usize, Buf, E>;
    const KIND: OpKind = OpKind::Recv;

    fn op(&mut self) -> Op<'_, B::Fd> {
        let len = clamp_len(self.buf.bytes_total());
        Op::Recv {
            fd: &self.fd,
            buf: self.buf.stable_mut_ptr(),
            len,
            flags: 0,
        }
    }

    fn finish<E: From<Error>>(mut self, result: Result<u32, E>) -> Self::Output<E> {
        let result = result.map(|n| {
            // SAFETY: the backend reported `n` bytes received into the start
            // of the buffer, and `n <= bytes_total`.
            unsafe { self.buf.set_init(n as usize) };
            n as usize
        });
        BufResult::new(result, self.buf)
    }
}

impl<B: Backend, Buf: IoBuf> OpSpec<B> for SocketSend<B, Buf> {
    type Output<E> = BufResult<usize, Buf, E>;
    const KIND: OpKind = OpKind::Send;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Send {
            fd: &self.fd,
            buf: self.buf.stable_ptr(),
            len: clamp_len(self.buf.bytes_init()),
            flags: libc::MSG_NOSIGNAL,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        BufResult::new(result.map(|n| n as usize), self.buf)
    }
}

impl<B: Backend> OpSpec<B> for Readiness<B> {
    type Output<E> = Result<u32, E>;
    const KIND: OpKind = OpKind::PollAdd;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::PollAdd {
            fd: &self.fd,
            events: self.events,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result
    }
}

impl<B: Backend> OpSpec<B> for Accept<B> {
    type Output<E> = Result<(B::Fd, SockAddr), E>;
    const KIND: OpKind = OpKind::Accept;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Accept {
            fd: &self.fd,
            peer: &mut self.peer,
            flags: libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
        }
    }

    fn deliver(&mut self, backend: &mut B, result: i32) {
        // SAFETY: `result` is the non-negative result of this Accept's
        // original completion, delivered exactly once.
        self.accepted = Some(unsafe { backend.adopt_fd(result) });
    }

    fn finish<E: From<Error>>(mut self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(|_| {
            let fd = self
                .accepted
                .take()
                .expect("accepted descriptor was delivered");
            (fd, self.peer)
        })
    }
}

impl<B: Backend> OpSpec<B> for Connect<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::Connect;

    fn prepare(&mut self) -> Result<()> {
        self.address.as_ref().map(|_| ()).map_err(Clone::clone)
    }

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Connect {
            fd: &self.fd,
            address: self.address.as_ref().expect("checked by prepare"),
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for OpenAt2<B> {
    type Output<E> = Result<B::Fd, E>;
    const KIND: OpKind = OpKind::OpenAt2;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::OpenAt2 {
            dir: self.dir.as_ref(),
            path: &self.path,
            how: &self.how,
        }
    }

    fn deliver(&mut self, backend: &mut B, result: i32) {
        // SAFETY: `result` is the non-negative result of this OpenAt2's
        // original completion, delivered exactly once.
        self.opened = Some(unsafe { backend.adopt_fd(result) });
    }

    fn finish<E: From<Error>>(mut self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(|_| self.opened.take().expect("opened descriptor was delivered"))
    }
}

impl<B: Backend> OpSpec<B> for Statx<B> {
    type Output<E> = Result<libc::statx, E>;
    const KIND: OpKind = OpKind::Statx;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Statx {
            dir: self.dir.as_ref(),
            path: &self.path,
            flags: self.flags,
            mask: self.mask,
            out: self.out.as_mut_ptr(),
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(|_| self.out[0])
    }
}

impl<B: Backend> OpSpec<B> for Fsync<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::Fsync;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Fsync {
            fd: &self.fd,
            datasync: self.datasync,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for RenameAt<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::RenameAt;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::RenameAt {
            old_dir: self.old_dir.as_ref(),
            old_path: &self.old_path,
            new_dir: self.new_dir.as_ref(),
            new_path: &self.new_path,
            flags: self.flags,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for UnlinkAt<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::UnlinkAt;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::UnlinkAt {
            dir: self.dir.as_ref(),
            path: &self.path,
            flags: self.flags,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for MkDirAt<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::MkDirAt;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::MkDirAt {
            dir: self.dir.as_ref(),
            path: &self.path,
            mode: self.mode,
        }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for SleepUntil<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::Timeout;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Timeout {
            deadline: self.deadline,
            timespec: &mut self.timespec,
        }
    }

    fn normalize(raw: i32) -> i32 {
        // An expired timeout is the expected outcome, not a failure.
        if raw == -libc::ETIME { 0 } else { raw }
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

impl<B: Backend> OpSpec<B> for Nop<B> {
    type Output<E> = Result<(), E>;
    const KIND: OpKind = OpKind::Nop;

    fn op(&mut self) -> Op<'_, B::Fd> {
        Op::Nop
    }

    fn finish<E: From<Error>>(self, result: Result<u32, E>) -> Self::Output<E> {
        result.map(drop)
    }
}

/// Operation constructors.
///
/// Every constructor takes the inputs by value (descriptors and paths are
/// cloned or copied into the operation), returns a lazy [`Operation`], and
/// gives every input back in its output, on success and on failure. Kernel
/// errors are converted into the scope's error type with `From<Error>`; keep
/// the [`Error`] in your error type if you need the errno.
impl<B: Backend> Reactor<B> {
    /// Read up to `buf.bytes_total()` bytes at `offset` (`pread`).
    ///
    /// Resolves to the number of bytes read (0 at end of file), with the
    /// buffer's initialized length extended to cover them. Short reads are
    /// returned as is. Offsets above `i64::MAX` fail with
    /// [`ErrorKind::InvalidInput`].
    pub fn read_at<Buf: IoBufMut, S: Scope>(
        &self,
        fd: &B::Fd,
        offset: u64,
        buf: Buf,
        scope: &S,
    ) -> Operation<'_, B, ReadAt<B, Buf>, S> {
        let data = ReadAt {
            fd: fd.clone(),
            offset,
            buf,
        };
        Operation::new(self, data, scope, ())
    }

    /// Write the initialized bytes of `buf` at `offset` (`pwrite`).
    ///
    /// Resolves to the number of bytes written; short writes are returned
    /// as is. Offsets above `i64::MAX` fail with [`ErrorKind::InvalidInput`].
    pub fn write_at<Buf: IoBuf, S: Scope>(
        &self,
        fd: &B::Fd,
        offset: u64,
        buf: Buf,
        scope: &S,
    ) -> Operation<'_, B, WriteAt<B, Buf>, S> {
        let data = WriteAt {
            fd: fd.clone(),
            offset,
            buf,
        };
        Operation::new(self, data, scope, ())
    }

    /// Gather the initialized bytes of every segment of `bufs` and write them
    /// contiguously at `offset` (`pwritev`).
    ///
    /// The segments and the kernel-visible iovec array stay owned by the
    /// operation until the kernel is done, with the same cancellation, drop,
    /// and offload semantics as [`write_at`](Self::write_at). Resolves to the
    /// number of bytes written; short writes are returned as is. Offsets
    /// above `i64::MAX`, more than 1024 segments, or more than `i32::MAX`
    /// total bytes fail with [`ErrorKind::InvalidInput`].
    pub fn writev_at<Bufs: IoBufs, S: Scope>(
        &self,
        fd: &B::Fd,
        offset: u64,
        bufs: Bufs,
        scope: &S,
    ) -> Operation<'_, B, WritevAt<B, Bufs>, S> {
        let data = WritevAt {
            fd: fd.clone(),
            offset,
            bufs,
            iovecs: Vec::new(),
        };
        Operation::new(self, data, scope, ())
    }

    /// Receive up to `buf.bytes_total()` bytes from a socket.
    ///
    /// Resolves to the number of bytes received; 0 means end of stream.
    pub fn recv<Buf: IoBufMut, S: Scope>(
        &self,
        fd: &B::Fd,
        buf: Buf,
        scope: &S,
    ) -> Operation<'_, B, SocketRecv<B, Buf>, S> {
        let data = SocketRecv {
            fd: fd.clone(),
            buf,
        };
        Operation::new(self, data, scope, ())
    }

    /// Send the initialized bytes of `buf` on a socket, with `MSG_NOSIGNAL`
    /// (a closed peer fails with `EPIPE` instead of raising `SIGPIPE`).
    ///
    /// Resolves to the number of bytes sent; short sends are returned as is.
    pub fn send<Buf: IoBuf, S: Scope>(
        &self,
        fd: &B::Fd,
        buf: Buf,
        scope: &S,
    ) -> Operation<'_, B, SocketSend<B, Buf>, S> {
        let data = SocketSend {
            fd: fd.clone(),
            buf,
        };
        Operation::new(self, data, scope, ())
    }

    /// Wait until `fd` reports any of the `POLL*` `events` (one shot).
    ///
    /// Resolves to the ready event mask, which may include `POLLERR`,
    /// `POLLHUP` or `POLLNVAL` even if not requested.
    pub fn readiness<S: Scope>(
        &self,
        fd: &B::Fd,
        events: u32,
        scope: &S,
    ) -> Operation<'_, B, Readiness<B>, S> {
        let data = Readiness {
            fd: fd.clone(),
            events,
        };
        Operation::new(self, data, scope, ())
    }

    /// Accept a connection on a listening socket.
    ///
    /// The new descriptor is nonblocking and close-on-exec. Resolves to the
    /// descriptor and the peer's raw address ([`SockAddr::decode`] it). The
    /// descriptor is owned by the operation from the moment the kernel
    /// reports it, so it is closed, never leaked, if the result is
    /// discarded.
    pub fn accept<S: Scope>(&self, fd: &B::Fd, scope: &S) -> Operation<'_, B, Accept<B>, S> {
        let data = Accept {
            fd: fd.clone(),
            peer: SockAddr::empty(),
            accepted: None,
        };
        Operation::new(self, data, scope, ())
    }

    /// Connect a socket to `address`.
    ///
    /// An address that cannot be encoded fails without being submitted. A
    /// refused or unreachable connection resolves to an error whose
    /// [`Error::raw_os_error`] is the kernel's errno.
    pub fn connect<S: Scope>(
        &self,
        fd: &B::Fd,
        address: &SocketAddress,
        scope: &S,
    ) -> Operation<'_, B, Connect<B>, S> {
        let data = Connect {
            fd: fd.clone(),
            address: SockAddr::encode(address),
        };
        Operation::new(self, data, scope, ())
    }

    /// Complete at `deadline` on the backend's clock.
    ///
    /// The deadline is absolute, so time spent before submission does not
    /// extend the sleep. An expired timer resolves to `Ok(())`.
    pub fn sleep_until<S: Scope>(
        &self,
        deadline: Instant,
        scope: &S,
    ) -> Operation<'_, B, SleepUntil<B>, S> {
        let data = SleepUntil {
            deadline,
            timespec: KernelTimespec::default(),
            _backend: std::marker::PhantomData,
        };
        Operation::new(self, data, scope, ())
    }

    /// Open `path` with `openat2(2)`, relative to `dir` (`None` means the
    /// current directory). Resolves to the new descriptor.
    pub fn open_at2<S: Scope>(
        &self,
        dir: Option<&B::Fd>,
        path: &CStr,
        how: OpenHow,
        scope: &S,
    ) -> Operation<'_, B, OpenAt2<B>, S> {
        let data = OpenAt2 {
            dir: dir.cloned(),
            path: path.to_owned(),
            how,
            opened: None,
        };
        Operation::new(self, data, scope, ())
    }

    /// `statx(2)`: resolves to the requested `STATX_*` fields of `path`.
    pub fn statx<S: Scope>(
        &self,
        dir: Option<&B::Fd>,
        path: &CStr,
        flags: i32,
        mask: u32,
        scope: &S,
    ) -> Operation<'_, B, Statx<B>, S> {
        let data = Statx {
            dir: dir.cloned(),
            path: path.to_owned(),
            flags,
            mask,
            // SAFETY: all-zero bytes are a valid `statx`.
            out: vec![unsafe { std::mem::zeroed() }],
        };
        Operation::new(self, data, scope, ())
    }

    /// `fsync(2)`, or `fdatasync(2)` if `datasync`.
    pub fn fsync<S: Scope>(
        &self,
        fd: &B::Fd,
        datasync: bool,
        scope: &S,
    ) -> Operation<'_, B, Fsync<B>, S> {
        let data = Fsync {
            fd: fd.clone(),
            datasync,
        };
        Operation::new(self, data, scope, ())
    }

    /// `renameat2(2)` with `RENAME_*` `flags`.
    pub fn rename_at<S: Scope>(
        &self,
        old_dir: Option<&B::Fd>,
        old_path: &CStr,
        new_dir: Option<&B::Fd>,
        new_path: &CStr,
        flags: u32,
        scope: &S,
    ) -> Operation<'_, B, RenameAt<B>, S> {
        let data = RenameAt {
            old_dir: old_dir.cloned(),
            old_path: old_path.to_owned(),
            new_dir: new_dir.cloned(),
            new_path: new_path.to_owned(),
            flags,
        };
        Operation::new(self, data, scope, ())
    }

    /// `unlinkat(2)`; pass `libc::AT_REMOVEDIR` in `flags` to remove a
    /// directory.
    pub fn unlink_at<S: Scope>(
        &self,
        dir: Option<&B::Fd>,
        path: &CStr,
        flags: i32,
        scope: &S,
    ) -> Operation<'_, B, UnlinkAt<B>, S> {
        let data = UnlinkAt {
            dir: dir.cloned(),
            path: path.to_owned(),
            flags,
        };
        Operation::new(self, data, scope, ())
    }

    /// `mkdirat(2)`. Requires Linux 5.15 on the io_uring backend.
    pub fn mkdir_at<S: Scope>(
        &self,
        dir: Option<&B::Fd>,
        path: &CStr,
        mode: u32,
        scope: &S,
    ) -> Operation<'_, B, MkDirAt<B>, S> {
        let data = MkDirAt {
            dir: dir.cloned(),
            path: path.to_owned(),
            mode,
        };
        Operation::new(self, data, scope, ())
    }

    /// An operation that does nothing; useful to test the reactor and to
    /// force a round trip through the backend.
    pub fn nop<S: Scope>(&self, scope: &S) -> Operation<'_, B, Nop<B>, S> {
        Operation::new(self, Nop(std::marker::PhantomData), scope, ())
    }
}
