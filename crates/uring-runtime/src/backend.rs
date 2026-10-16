//! The backend seam between the reactor and the operating system.
//!
//! The reactor owns operation state (buffers, addresses, paths, the in-flight
//! table and its fences) and talks to the kernel, or to a simulator, through
//! the narrow [`Backend`] trait: push raw operation descriptions, submit,
//! pop completions, wait. [`Host`] adds the synchronous, POSIX-shaped
//! resource operations (socket creation, bind, listen, ...) an application
//! needs, so application code never branches on which backend it runs on.
//!
//! # Ownership of operation memory
//!
//! An [`Op`] contains raw pointers and borrowed references into memory owned
//! by the reactor's in-flight entry. [`Backend::push`] is `unsafe`: the caller
//! promises that this memory stays valid, and is not otherwise accessed,
//! until the completion for that `user_data` has been returned by
//! [`Backend::pop`]. A backend may therefore hand the pointers to the kernel,
//! or (like a simulator) keep them and complete the operation later.
//!
//! # Example: a minimal backend
//!
//! ```
//! use std::collections::VecDeque;
//! use std::time::Duration;
//! use uring_runtime::backend::{
//!     Backend, BackendConfig, CompletionEntry, Op, OpKind, PushError, RemoteWaker,
//! };
//! use uring_runtime::{Result, SystemClock, SystemEntropy};
//!
//! /// Completes every `Nop` immediately with result 0.
//! #[derive(Default)]
//! struct Immediate(VecDeque<CompletionEntry>);
//!
//! impl Backend for Immediate {
//!     type Fd = i32;
//!     type Clock = SystemClock;
//!     type Entropy = SystemEntropy;
//!
//!     fn clock(&self) -> &SystemClock { &SystemClock }
//!     fn entropy(&self) -> &SystemEntropy { &SystemEntropy }
//!     fn init(&mut self, _: &BackendConfig) -> Result<()> { Ok(()) }
//!     unsafe fn push(&mut self, user_data: u64, op: &Op<'_, i32>) -> Result<(), PushError> {
//!         if op.kind() != OpKind::Nop {
//!             return Err(PushError::Unsupported);
//!         }
//!         self.0.push_back(CompletionEntry { user_data, result: 0, flags: 0 });
//!         Ok(())
//!     }
//!     fn cancel(&mut self, user_data: u64, _target: u64) -> Result<(), PushError> {
//!         self.0.push_back(CompletionEntry { user_data, result: -libc::ENOENT, flags: 0 });
//!         Ok(())
//!     }
//!     unsafe fn adopt_fd(&mut self, raw: i32) -> i32 { raw }
//!     fn submit(&mut self) -> Result<usize> { Ok(0) }
//!     fn pop(&mut self) -> Option<CompletionEntry> { self.0.pop_front() }
//!     fn wait(&mut self, _: Duration) -> Result<()> { Ok(()) }
//!     fn remote_waker(&self) -> Result<RemoteWaker> { Ok(RemoteWaker::new(|| {})) }
//!     fn supports(&self, kind: OpKind) -> bool { kind == OpKind::Nop }
//! }
//!
//! let mut backend = Immediate::default();
//! backend.init(&BackendConfig::new(8)).unwrap();
//! // SAFETY: `Nop` references no memory.
//! unsafe { backend.push(7, &Op::Nop) }.unwrap();
//! assert_eq!(backend.pop().unwrap().user_data, 7);
//! ```

use std::ffi::CStr;
use std::fmt;
use std::net::Shutdown;
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::{Clock, Entropy, Error, Result, SockAddr, SocketAddress};

/// Settings passed to [`Backend::init`].
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BackendConfig {
    /// The maximum number of operations (including cancel requests) the
    /// reactor will have in flight at once. A ring backend sizes its
    /// submission and completion queues from this.
    pub capacity: u32,
}

impl BackendConfig {
    /// Create a configuration for `capacity` concurrent operations.
    pub fn new(capacity: u32) -> Self {
        Self { capacity }
    }
}

/// A completed operation, as reported by [`Backend::pop`].
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub struct CompletionEntry {
    /// The `user_data` passed to [`Backend::push`] or [`Backend::cancel`].
    pub user_data: u64,
    /// Non-negative on success; a negated errno on failure.
    pub result: i32,
    /// Backend-specific completion flags (io_uring `cqe.flags`).
    pub flags: u32,
}

impl CompletionEntry {
    /// Convert [`result`](CompletionEntry::result) into a `Result`.
    pub fn into_result(self) -> Result<u32> {
        u32::try_from(self.result).map_err(|_| Error::from_errno(self.result))
    }
}

/// Why [`Backend::push`] or [`Backend::cancel`] did not accept an operation.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum PushError {
    /// The submission queue is full. Submit, reap completions and retry.
    Full,
    /// The backend does not implement this operation kind.
    Unsupported,
    /// The operation was rejected.
    Failed(Error),
}

impl fmt::Display for PushError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Full => f.write_str("submission queue full"),
            Self::Unsupported => f.write_str("operation unsupported by backend"),
            Self::Failed(error) => write!(f, "operation rejected: {error}"),
        }
    }
}

impl std::error::Error for PushError {}

impl From<Error> for PushError {
    fn from(error: Error) -> Self {
        Self::Failed(error)
    }
}

impl From<PushError> for Error {
    fn from(error: PushError) -> Self {
        match error {
            PushError::Full => Error::new(crate::ErrorKind::Overloaded),
            PushError::Unsupported => Error::new(crate::ErrorKind::Unsupported),
            PushError::Failed(error) => error,
        }
    }
}

/// A cloneable, thread-safe handle that wakes a backend blocked in
/// [`Backend::wait`] (an eventfd write for io_uring).
#[derive(Clone)]
pub struct RemoteWaker(Arc<dyn Fn() + Send + Sync>);

impl RemoteWaker {
    /// Wrap a wake function. It must be cheap, non-blocking and idempotent.
    pub fn new(wake: impl Fn() + Send + Sync + 'static) -> Self {
        Self(Arc::new(wake))
    }

    /// Wake the backend. Wakes may be coalesced.
    pub fn wake(&self) {
        (self.0)();
    }
}

impl fmt::Debug for RemoteWaker {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("RemoteWaker")
    }
}

/// A `struct __kernel_timespec`, as read by io_uring timeouts.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct KernelTimespec {
    /// Seconds.
    pub tv_sec: i64,
    /// Nanoseconds, `0..1_000_000_000`.
    pub tv_nsec: i64,
}

/// A `struct open_how`, as read by `openat2(2)`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct OpenHow {
    /// `O_*` flags.
    pub flags: u64,
    /// File mode for `O_CREAT`/`O_TMPFILE`.
    pub mode: u64,
    /// `RESOLVE_*` flags.
    pub resolve: u64,
}

/// A raw operation description handed to [`Backend::push`].
///
/// Building an `Op` is safe; every pointer and reference in it must remain
/// valid until the operation's completion has been popped, as required by
/// [`Backend::push`]. Directory arguments of `None` mean `AT_FDCWD`.
#[non_exhaustive]
#[derive(Debug)]
pub enum Op<'a, Fd> {
    /// Do nothing; completes with result 0.
    Nop,
    /// `pread(fd, buf, len, offset)`. An offset of `u64::MAX` uses and
    /// advances the file position.
    Read {
        /// Source descriptor.
        fd: &'a Fd,
        /// File offset.
        offset: u64,
        /// Destination, valid for `len` writable bytes.
        buf: *mut u8,
        /// Maximum bytes to read.
        len: u32,
    },
    /// `pwrite(fd, buf, len, offset)`. An offset of `u64::MAX` uses and
    /// advances the file position.
    Write {
        /// Destination descriptor.
        fd: &'a Fd,
        /// File offset.
        offset: u64,
        /// Source, valid for `len` readable bytes.
        buf: *const u8,
        /// Bytes to write.
        len: u32,
    },
    /// `recv(fd, buf, len, flags)`.
    Recv {
        /// Socket.
        fd: &'a Fd,
        /// Destination, valid for `len` writable bytes.
        buf: *mut u8,
        /// Maximum bytes to receive.
        len: u32,
        /// `MSG_*` flags.
        flags: i32,
    },
    /// `send(fd, buf, len, flags)`.
    Send {
        /// Socket.
        fd: &'a Fd,
        /// Source, valid for `len` readable bytes.
        buf: *const u8,
        /// Bytes to send.
        len: u32,
        /// `MSG_*` flags.
        flags: i32,
    },
    /// One-shot readiness poll; completes with the ready `POLL*` events.
    PollAdd {
        /// Descriptor to poll.
        fd: &'a Fd,
        /// `POLL*` event mask.
        events: u32,
    },
    /// `accept4(fd, peer, flags)`; completes with the new raw descriptor,
    /// which the reactor takes ownership of.
    Accept {
        /// Listening socket.
        fd: &'a Fd,
        /// Where to store the peer address, or null to discard it.
        peer: *mut SockAddr,
        /// `SOCK_*` flags (for example `SOCK_CLOEXEC`).
        flags: i32,
    },
    /// `connect(fd, address)`.
    Connect {
        /// Socket.
        fd: &'a Fd,
        /// Encoded remote address.
        address: &'a SockAddr,
    },
    /// Complete at an absolute monotonic deadline with `-ETIME`.
    Timeout {
        /// The deadline on the backend's [`Clock`].
        deadline: Instant,
        /// Reactor-owned scratch storage. A kernel backend writes the
        /// absolute `CLOCK_MONOTONIC` time for `deadline` here and passes
        /// the pointer to the kernel.
        timespec: *mut KernelTimespec,
    },
    /// `openat2(dir, path, how)`; completes with the new raw descriptor.
    OpenAt2 {
        /// Directory for relative paths.
        dir: Option<&'a Fd>,
        /// Path.
        path: &'a CStr,
        /// Open parameters.
        how: &'a OpenHow,
    },
    /// `statx(dir, path, flags, mask, out)`.
    Statx {
        /// Directory for relative paths, or the target with `AT_EMPTY_PATH`.
        dir: Option<&'a Fd>,
        /// Path.
        path: &'a CStr,
        /// `AT_*` flags.
        flags: i32,
        /// `STATX_*` mask.
        mask: u32,
        /// Output, valid for writes of one `statx`.
        out: *mut libc::statx,
    },
    /// `fsync(fd)` or `fdatasync(fd)`.
    Fsync {
        /// Descriptor.
        fd: &'a Fd,
        /// Use `fdatasync` semantics.
        datasync: bool,
    },
    /// `renameat2(old_dir, old_path, new_dir, new_path, flags)`.
    RenameAt {
        /// Directory for a relative `old_path`.
        old_dir: Option<&'a Fd>,
        /// Existing path.
        old_path: &'a CStr,
        /// Directory for a relative `new_path`.
        new_dir: Option<&'a Fd>,
        /// New path.
        new_path: &'a CStr,
        /// `RENAME_*` flags.
        flags: u32,
    },
    /// `unlinkat(dir, path, flags)`.
    UnlinkAt {
        /// Directory for relative paths.
        dir: Option<&'a Fd>,
        /// Path.
        path: &'a CStr,
        /// `0` or `AT_REMOVEDIR`.
        flags: i32,
    },
    /// `mkdirat(dir, path, mode)`. Requires Linux 5.15 with io_uring.
    MkDirAt {
        /// Directory for relative paths.
        dir: Option<&'a Fd>,
        /// Path.
        path: &'a CStr,
        /// Permission bits.
        mode: u32,
    },
}

/// The kind of an [`Op`], for capability checks and fault injection.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub enum OpKind {
    /// [`Op::Nop`].
    Nop,
    /// [`Op::Read`].
    Read,
    /// [`Op::Write`].
    Write,
    /// [`Op::Recv`].
    Recv,
    /// [`Op::Send`].
    Send,
    /// [`Op::PollAdd`].
    PollAdd,
    /// [`Op::Accept`].
    Accept,
    /// [`Op::Connect`].
    Connect,
    /// [`Op::Timeout`].
    Timeout,
    /// [`Op::OpenAt2`].
    OpenAt2,
    /// [`Op::Statx`].
    Statx,
    /// [`Op::Fsync`].
    Fsync,
    /// [`Op::RenameAt`].
    RenameAt,
    /// [`Op::UnlinkAt`].
    UnlinkAt,
    /// [`Op::MkDirAt`].
    MkDirAt,
    /// An asynchronous cancel request ([`Backend::cancel`]).
    Cancel,
}

impl OpKind {
    /// Every kind, in declaration order.
    pub const ALL: [OpKind; 16] = [
        Self::Nop,
        Self::Read,
        Self::Write,
        Self::Recv,
        Self::Send,
        Self::PollAdd,
        Self::Accept,
        Self::Connect,
        Self::Timeout,
        Self::OpenAt2,
        Self::Statx,
        Self::Fsync,
        Self::RenameAt,
        Self::UnlinkAt,
        Self::MkDirAt,
        Self::Cancel,
    ];
}

impl<Fd> Op<'_, Fd> {
    /// The operation's kind.
    pub fn kind(&self) -> OpKind {
        match self {
            Self::Nop => OpKind::Nop,
            Self::Read { .. } => OpKind::Read,
            Self::Write { .. } => OpKind::Write,
            Self::Recv { .. } => OpKind::Recv,
            Self::Send { .. } => OpKind::Send,
            Self::PollAdd { .. } => OpKind::PollAdd,
            Self::Accept { .. } => OpKind::Accept,
            Self::Connect { .. } => OpKind::Connect,
            Self::Timeout { .. } => OpKind::Timeout,
            Self::OpenAt2 { .. } => OpKind::OpenAt2,
            Self::Statx { .. } => OpKind::Statx,
            Self::Fsync { .. } => OpKind::Fsync,
            Self::RenameAt { .. } => OpKind::RenameAt,
            Self::UnlinkAt { .. } => OpKind::UnlinkAt,
            Self::MkDirAt { .. } => OpKind::MkDirAt,
        }
    }
}

/// An asynchronous I/O backend driven by the reactor.
///
/// All methods are called from the reactor's thread. The protocol is:
/// [`init`](Backend::init) once, then any interleaving of
/// [`push`](Backend::push)/[`cancel`](Backend::cancel) (queue),
/// [`submit`](Backend::submit) (start queued work),
/// [`pop`](Backend::pop) (reap) and [`wait`](Backend::wait) (block).
/// Every accepted push and cancel produces exactly one completion.
pub trait Backend: 'static {
    /// The descriptor type ([`Fd`](crate::Fd) for the kernel backend).
    type Fd: Clone + fmt::Debug + 'static;
    /// The time source. Must be cheap to clone (see [`clock`](crate::clock)).
    type Clock: Clock + Clone;
    /// The randomness source.
    type Entropy: Entropy;

    /// The backend's clock. Timeouts are interpreted on this clock.
    fn clock(&self) -> &Self::Clock;

    /// The backend's entropy source.
    fn entropy(&self) -> &Self::Entropy;

    /// Prepare for use with at most `config.capacity` in-flight operations.
    ///
    /// Called exactly once, before any other method except
    /// [`clock`](Backend::clock), [`entropy`](Backend::entropy) and
    /// [`supports`](Backend::supports).
    fn init(&mut self, config: &BackendConfig) -> Result<()>;

    /// Queue `op`, tagging its completion with `user_data`.
    ///
    /// The backend must not use a `user_data` value to identify anything but
    /// the corresponding completion; the reactor chooses all values.
    ///
    /// # Safety
    ///
    /// Every raw pointer in `op`, and the referent of every reference in
    /// `op`, must remain valid for the access the operation describes, and
    /// must not be accessed by anything else in a conflicting way, until a
    /// completion with this `user_data` has been returned by
    /// [`pop`](Backend::pop). If `push` returns an error, the operation was
    /// not queued and no completion will be produced.
    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, Self::Fd>) -> Result<(), PushError>;

    /// Queue a request to cancel the operation tagged `target`. The request
    /// itself completes with `user_data` (`0`, `-ENOENT` or `-EALREADY`, as
    /// for `IORING_OP_ASYNC_CANCEL`); the target still produces its own
    /// completion.
    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError>;

    /// Take ownership of the descriptor reported by a successful
    /// [`Op::Accept`] or [`Op::OpenAt2`] completion (`result >= 0`).
    ///
    /// The reactor calls this as soon as the completion is popped, so the
    /// descriptor is closed (by dropping the returned value) if nobody
    /// collects it.
    ///
    /// # Safety
    ///
    /// `raw` must be the `result` of a completion, popped from this backend,
    /// of an `Accept` or `OpenAt2` operation, and must be adopted at most
    /// once.
    unsafe fn adopt_fd(&mut self, raw: i32) -> Self::Fd;

    /// Start every queued operation, returning how many were submitted.
    fn submit(&mut self) -> Result<usize>;

    /// Take the next available completion without blocking.
    fn pop(&mut self) -> Option<CompletionEntry>;

    /// Block until a completion is available, a [`RemoteWaker`] fires, or
    /// `timeout` elapses. Spurious returns are allowed; `EINTR` is not an
    /// error.
    fn wait(&mut self, timeout: Duration) -> Result<()>;

    /// A handle other threads use to interrupt [`wait`](Backend::wait).
    fn remote_waker(&self) -> Result<RemoteWaker>;

    /// Whether operations of `kind` are available (after
    /// [`init`](Backend::init), for kernel backends).
    fn supports(&self, kind: OpKind) -> bool;
}

/// Synchronous, POSIX-shaped resource operations.
///
/// These calls never block: sockets are created nonblocking and close-on-exec,
/// and `try_*` calls return `EAGAIN` (see [`Error::is_would_block`]) instead of
/// waiting. Readiness is awaited through the reactor's poll operation.
pub trait Host: Backend {
    /// `socket(domain, ty | SOCK_NONBLOCK | SOCK_CLOEXEC, protocol)`.
    fn socket(&self, domain: i32, ty: i32, protocol: i32) -> Result<Self::Fd>;

    /// `bind(fd, address)`.
    fn bind(&self, fd: &Self::Fd, address: &SocketAddress) -> Result<()>;

    /// `listen(fd, backlog)`.
    fn listen(&self, fd: &Self::Fd, backlog: i32) -> Result<()>;

    /// `getsockname(fd)`.
    fn local_addr(&self, fd: &Self::Fd) -> Result<SocketAddress>;

    /// `getpeername(fd)`.
    fn peer_addr(&self, fd: &Self::Fd) -> Result<SocketAddress>;

    /// `shutdown(fd, how)`.
    fn shutdown(&self, fd: &Self::Fd, how: Shutdown) -> Result<()>;

    /// Set `TCP_NODELAY`.
    fn set_nodelay(&self, fd: &Self::Fd, enabled: bool) -> Result<()>;

    /// Nonblocking `send(fd, buf, flags | MSG_DONTWAIT | MSG_NOSIGNAL)`.
    fn try_send(&self, fd: &Self::Fd, buf: &[u8], flags: i32) -> Result<usize>;

    /// Nonblocking `recv(fd, buf, flags | MSG_DONTWAIT)`. Returns 0 at end of
    /// stream.
    fn try_recv(&self, fd: &Self::Fd, buf: &mut [u8], flags: i32) -> Result<usize>;

    /// `poll(fd, events, 0)`: the currently ready `POLL*` events (possibly
    /// including `POLLERR`/`POLLHUP`), without waiting.
    fn poll_now(&self, fd: &Self::Fd, events: i16) -> Result<i16>;

    /// `pipe2(O_NONBLOCK | O_CLOEXEC)`, returning `(read end, write end)`.
    fn pipe(&self) -> Result<(Self::Fd, Self::Fd)>;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{ErrorKind, SystemClock, SystemEntropy};
    use std::collections::VecDeque;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Records pushes and completes reads by filling the buffer later,
    /// exercising the deferred-pointer contract.
    #[derive(Default)]
    struct Deferred {
        reads: Vec<(u64, *mut u8, u32)>,
        done: VecDeque<CompletionEntry>,
    }

    impl Backend for Deferred {
        type Fd = u32;
        type Clock = SystemClock;
        type Entropy = SystemEntropy;

        fn clock(&self) -> &SystemClock {
            &SystemClock
        }

        fn entropy(&self) -> &SystemEntropy {
            &SystemEntropy
        }

        fn init(&mut self, config: &BackendConfig) -> Result<()> {
            if config.capacity == 0 {
                return Err(Error::new(ErrorKind::InvalidConfiguration));
            }
            Ok(())
        }

        unsafe fn push(&mut self, user_data: u64, op: &Op<'_, u32>) -> Result<(), PushError> {
            match *op {
                Op::Read { buf, len, .. } => self.reads.push((user_data, buf, len)),
                Op::Nop => self.done.push_back(CompletionEntry {
                    user_data,
                    result: 0,
                    flags: 0,
                }),
                _ => return Err(PushError::Unsupported),
            }
            Ok(())
        }

        fn cancel(&mut self, _: u64, _: u64) -> Result<(), PushError> {
            Err(PushError::Full)
        }

        unsafe fn adopt_fd(&mut self, raw: i32) -> u32 {
            raw as u32
        }

        fn submit(&mut self) -> Result<usize> {
            let n = self.reads.len();
            for (user_data, buf, len) in self.reads.drain(..) {
                // SAFETY: push's contract keeps `buf` valid until popped.
                unsafe { buf.write_bytes(0xab, len as usize) };
                self.done.push_back(CompletionEntry {
                    user_data,
                    result: len as i32,
                    flags: 0,
                });
            }
            Ok(n)
        }

        fn pop(&mut self) -> Option<CompletionEntry> {
            self.done.pop_front()
        }

        fn wait(&mut self, _: Duration) -> Result<()> {
            Ok(())
        }

        fn remote_waker(&self) -> Result<RemoteWaker> {
            Ok(RemoteWaker::new(|| {}))
        }

        fn supports(&self, kind: OpKind) -> bool {
            matches!(kind, OpKind::Read | OpKind::Nop)
        }
    }

    #[test]
    fn deferred_backend_round_trip() {
        let mut backend = Deferred::default();
        assert!(backend.init(&BackendConfig::new(0)).is_err());
        backend.init(&BackendConfig::new(4)).unwrap();
        let mut buf = vec![0u8; 4];
        let fd = 3;
        let op = Op::Read {
            fd: &fd,
            offset: 0,
            buf: buf.as_mut_ptr(),
            len: 4,
        };
        assert_eq!(op.kind(), OpKind::Read);
        // SAFETY: `buf` outlives the completion popped below.
        unsafe { backend.push(1, &op) }.unwrap();
        assert!(backend.pop().is_none());
        assert_eq!(backend.submit().unwrap(), 1);
        let entry = backend.pop().unwrap();
        assert_eq!(entry.user_data, 1);
        assert_eq!(entry.into_result(), Ok(4));
        assert_eq!(buf, [0xab; 4]);

        // SAFETY: Fsync references only `fd`, which outlives the call.
        let rejected = unsafe {
            backend.push(
                2,
                &Op::Fsync {
                    fd: &fd,
                    datasync: true,
                },
            )
        };
        assert_eq!(rejected, Err(PushError::Unsupported));
        assert!(!backend.supports(OpKind::Fsync));
        assert_eq!(backend.cancel(3, 1), Err(PushError::Full));
    }

    #[test]
    fn completion_results() {
        let ok = CompletionEntry {
            user_data: 0,
            result: 5,
            flags: 0,
        };
        assert_eq!(ok.into_result(), Ok(5));
        let failed = CompletionEntry {
            result: -libc::ECANCELED,
            ..ok
        };
        let error = failed.into_result().unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Os);
        assert_eq!(error.raw_os_error(), Some(libc::ECANCELED));
    }

    #[test]
    fn push_error_conversions() {
        assert_eq!(Error::from(PushError::Full).kind(), ErrorKind::Overloaded);
        assert_eq!(
            Error::from(PushError::Unsupported).kind(),
            ErrorKind::Unsupported
        );
        let inner = Error::from_errno(libc::EBADF);
        assert_eq!(Error::from(PushError::from(inner.clone())), inner);
        assert!(PushError::Full.to_string().contains("full"));
    }

    #[test]
    fn remote_waker_is_send_sync_and_shared() {
        fn assert_send_sync<T: Send + Sync>(_: &T) {}
        let count = Arc::new(AtomicUsize::new(0));
        let waker = {
            let count = count.clone();
            RemoteWaker::new(move || {
                count.fetch_add(1, Ordering::SeqCst);
            })
        };
        assert_send_sync(&waker);
        let clone = waker.clone();
        std::thread::spawn(move || clone.wake()).join().unwrap();
        waker.wake();
        assert_eq!(count.load(Ordering::SeqCst), 2);
        assert_eq!(format!("{waker:?}"), "RemoteWaker");
    }

    #[test]
    fn op_kinds_are_distinct() {
        let fd = 0u32;
        let path = c"p";
        let how = OpenHow::default();
        let address = SockAddr::empty();
        let mut timespec = KernelTimespec::default();
        let ops: Vec<Op<'_, u32>> = vec![
            Op::Nop,
            Op::Write {
                fd: &fd,
                offset: 0,
                buf: std::ptr::null(),
                len: 0,
            },
            Op::Recv {
                fd: &fd,
                buf: std::ptr::null_mut(),
                len: 0,
                flags: 0,
            },
            Op::Send {
                fd: &fd,
                buf: std::ptr::null(),
                len: 0,
                flags: 0,
            },
            Op::PollAdd { fd: &fd, events: 1 },
            Op::Accept {
                fd: &fd,
                peer: std::ptr::null_mut(),
                flags: 0,
            },
            Op::Connect {
                fd: &fd,
                address: &address,
            },
            Op::Timeout {
                deadline: Instant::now(),
                timespec: &mut timespec,
            },
            Op::OpenAt2 {
                dir: None,
                path,
                how: &how,
            },
            Op::Statx {
                dir: Some(&fd),
                path,
                flags: 0,
                mask: 0,
                out: std::ptr::null_mut(),
            },
            Op::RenameAt {
                old_dir: None,
                old_path: path,
                new_dir: None,
                new_path: path,
                flags: 0,
            },
            Op::UnlinkAt {
                dir: None,
                path,
                flags: 0,
            },
            Op::MkDirAt {
                dir: None,
                path,
                mode: 0o755,
            },
        ];
        let mut kinds: Vec<OpKind> = ops.iter().map(Op::kind).collect();
        kinds.sort();
        kinds.dedup();
        assert_eq!(kinds.len(), ops.len());
        assert!(kinds.iter().all(|kind| OpKind::ALL.contains(kind)));
        assert!(format!("{:?}", ops[0]).contains("Nop"));
    }
}
