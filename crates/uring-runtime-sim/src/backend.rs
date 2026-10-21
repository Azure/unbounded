//! [`SimBackend`]: the [`Backend`] and [`Host`] implementation.
//!
//! # Execution model
//!
//! Pushed operations are queued. Each *turn*, every submitted operation that
//! is not delayed is executed once against the world, in push order:
//!
//! - an operation that would block (a `recv` with no data, a `send` into a
//!   full or partitioned stream, an `accept` with no pending connection, a
//!   `poll` with no ready events, a partitioned `connect`, a timeout whose
//!   deadline has not passed) stays queued and is retried next turn;
//! - anything else produces its completion, which [`pop`](Backend::pop)
//!   returns in order.
//!
//! [`submit`](Backend::submit) and [`wait`](Backend::wait) each run one turn.
//! [`pop`](Backend::pop) additionally completes expired timeouts, so timers
//! fire as soon as the [`SimClock`] passes their deadline.
//!
//! `wait` never sleeps on the host. By default it runs one turn and returns.
//! With [`set_auto_advance`](SimBackend::set_auto_advance), if that turn
//! produces nothing, it advances the shared clock to the earliest pending
//! timeout deadline (at most by the wait's own timeout) and runs another
//! turn, so a program that is idle until a timer fires makes progress
//! without a test driver advancing time by hand.
//!
//! # Results that create resources
//!
//! `Accept` and `OpenAt2` complete with the new resource's id as a
//! nonnegative result, exactly like a kernel file descriptor number. Turn it
//! into a [`SimFd`] with [`SimBackend::try_adopt`] (or
//! [`World::adopt`]); the reactor does this itself through
//! [`Backend::adopt_fd`]. Until then it stays open and counted by
//! [`World::live_handles`].

use std::collections::{BTreeMap, VecDeque};
use std::ffi::CStr;
use std::fmt;
use std::net::Shutdown;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::time::{Duration, Instant};

use uring_runtime::backend::{
    BackendConfig, CompletionEntry, Op, OpKind, OpenHow, PushError, RemoteWaker,
};
use uring_runtime::{Backend, Clock, Error, Host, Result, SockAddr, SocketAddress};

use crate::disk::CrashWatch;
use crate::fault::{Fault, FaultTarget, SyncOp};
use crate::net::DEFAULT_PIPE_CAPACITY;
use crate::trace::EventKind;
use crate::world::{Resource, SimFd, State, World, errno};
use crate::{SimClock, SimEntropy};

/// The most completions that may be queued through
/// [`SimBackend::inject_completion`].
pub const MAX_INJECTED: usize = 16384;

/// A deterministic [`Backend`] and [`Host`] over a shared [`World`].
///
/// Create one per simulated worker; backends sharing a world (and usually a
/// clock) can talk to each other through it. See the [module
/// docs](self) for the execution model.
///
/// ```
/// use std::time::Duration;
/// use uring_runtime::backend::{BackendConfig, Op};
/// use uring_runtime::{Backend, Clock, Host};
/// use uring_runtime_sim::{DEFAULT_ENTROPY_DOMAIN, SimBackend, SimClock, SimEntropy, World};
///
/// let world = World::new();
/// let clock = SimClock::new(1);
/// let entropy = SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 1, 0);
/// let mut backend = SimBackend::new(&world, clock.clone(), entropy);
/// backend.init(&BackendConfig::new(16)).unwrap();
///
/// let (reader, writer) = backend.pipe().unwrap();
/// let mut buf = [0u8; 5];
/// let read = Op::Read { fd: &reader, offset: u64::MAX, buf: buf.as_mut_ptr(), len: 5 };
/// // SAFETY: `buf` and `reader` outlive the completion popped below.
/// unsafe { backend.push(1, &read) }.unwrap();
/// backend.submit().unwrap();
/// assert!(backend.pop().is_none()); // nothing to read yet
///
/// writer.pipe_write(b"hello").unwrap();
/// backend.wait(Duration::from_millis(1)).unwrap();
/// let done = backend.pop().unwrap();
/// assert_eq!((done.user_data, done.result), (1, 5));
/// assert_eq!(&buf, b"hello");
/// ```
pub struct SimBackend {
    world: World,
    clock: SimClock,
    entropy: SimEntropy,
    endpoint: Option<SocketAddress>,
    capacity: usize,
    auto_advance: bool,
    next_sequence: u64,
    /// Queued operations by push order.
    pending: BTreeMap<u64, Pending>,
    completed: VecDeque<CompletionEntry>,
}

/// An operation description with everything the backend may copy copied.
/// Buffers and outputs stay raw pointers into reactor-owned memory.
enum Owned {
    Nop,
    Read {
        fd: SimFd,
        offset: u64,
        buf: *mut u8,
        len: usize,
    },
    Write {
        fd: SimFd,
        offset: u64,
        buf: *const u8,
        len: usize,
    },
    /// A vectored write; segments are gathered at execution time.
    Writev {
        fd: SimFd,
        offset: u64,
        iovecs: *const libc::iovec,
        count: usize,
    },
    Recv {
        fd: SimFd,
        buf: *mut u8,
        len: usize,
        flags: i32,
    },
    Send {
        fd: SimFd,
        buf: *const u8,
        len: usize,
        flags: i32,
    },
    Poll {
        fd: SimFd,
        events: u32,
    },
    Accept {
        fd: SimFd,
        peer: *mut SockAddr,
    },
    Connect {
        fd: SimFd,
        address: Result<SocketAddress>,
    },
    Timeout {
        deadline: Instant,
    },
    Open {
        dir: Option<SimFd>,
        path: PathBuf,
        how: OpenHow,
    },
    Statx {
        dir: Option<SimFd>,
        path: PathBuf,
        flags: i32,
        out: *mut libc::statx,
    },
    Fsync {
        fd: SimFd,
    },
    Rename {
        old_dir: Option<SimFd>,
        old_path: PathBuf,
        new_dir: Option<SimFd>,
        new_path: PathBuf,
        flags: u32,
    },
    Unlink {
        dir: Option<SimFd>,
        path: PathBuf,
        flags: i32,
    },
    Mkdir {
        dir: Option<SimFd>,
        path: PathBuf,
        mode: u32,
    },
}

struct Pending {
    user_data: u64,
    kind: OpKind,
    op: Owned,
    submitted: bool,
    delay: usize,
    limit: usize,
    error: Option<i32>,
    hold: usize,
    result: Option<i32>,
    crash: Rc<CrashWatch>,
}

fn owned_path(path: &CStr) -> PathBuf {
    PathBuf::from(std::ffi::OsStr::from_bytes(path.to_bytes()))
}

/// Build a byte slice from a raw operation buffer.
///
/// # Safety
///
/// `buf` must be valid for `len` bytes, as guaranteed by `Backend::push`.
unsafe fn slice<'a>(buf: *const u8, len: usize) -> Result<&'a [u8]> {
    if len == 0 {
        return Ok(&[]);
    }
    if buf.is_null() {
        return Err(errno(libc::EFAULT));
    }
    // SAFETY: the caller guarantees validity for `len` bytes.
    Ok(unsafe { std::slice::from_raw_parts(buf, len) })
}

/// Build an iovec slice from a raw operation array.
///
/// # Safety
///
/// `iovecs` must be valid for `count` entries, as guaranteed by
/// `Backend::push`.
unsafe fn iovec_slice<'a>(iovecs: *const libc::iovec, count: usize) -> Result<&'a [libc::iovec]> {
    if count == 0 {
        return Ok(&[]);
    }
    if iovecs.is_null() {
        return Err(errno(libc::EFAULT));
    }
    // SAFETY: the caller guarantees validity for `count` entries.
    Ok(unsafe { std::slice::from_raw_parts(iovecs, count) })
}

/// Mutable counterpart of [`slice`].
///
/// # Safety
///
/// `buf` must be valid and unaliased for `len` writable bytes.
unsafe fn slice_mut<'a>(buf: *mut u8, len: usize) -> Result<&'a mut [u8]> {
    if len == 0 {
        return Ok(&mut []);
    }
    if buf.is_null() {
        return Err(errno(libc::EFAULT));
    }
    // SAFETY: the caller guarantees exclusive validity for `len` bytes.
    Ok(unsafe { std::slice::from_raw_parts_mut(buf, len) })
}

fn result_of(value: Result<i32>) -> i32 {
    value.unwrap_or_else(|error| -error.raw_os_error().unwrap_or(libc::EIO))
}

impl Owned {
    fn from_op(op: &Op<'_, SimFd>) -> Result<Self, PushError> {
        let dir = |dir: Option<&SimFd>| dir.cloned();
        Ok(match *op {
            Op::Nop => Self::Nop,
            Op::Read {
                fd,
                offset,
                buf,
                len,
            } => Self::Read {
                fd: fd.clone(),
                offset,
                buf,
                len: len as usize,
            },
            Op::Write {
                fd,
                offset,
                buf,
                len,
            } => Self::Write {
                fd: fd.clone(),
                offset,
                buf,
                len: len as usize,
            },
            Op::Writev {
                fd,
                offset,
                iovecs,
                count,
            } => Self::Writev {
                fd: fd.clone(),
                offset,
                iovecs,
                count: count as usize,
            },
            Op::Recv {
                fd,
                buf,
                len,
                flags,
            } => Self::Recv {
                fd: fd.clone(),
                buf,
                len: len as usize,
                flags,
            },
            Op::Send {
                fd,
                buf,
                len,
                flags,
            } => Self::Send {
                fd: fd.clone(),
                buf,
                len: len as usize,
                flags,
            },
            Op::PollAdd { fd, events } => Self::Poll {
                fd: fd.clone(),
                events,
            },
            Op::Accept { fd, peer, .. } => Self::Accept {
                fd: fd.clone(),
                peer,
            },
            Op::Connect { fd, address } => Self::Connect {
                fd: fd.clone(),
                address: address.decode().map_err(|_| errno(libc::EINVAL)),
            },
            Op::Timeout { deadline, .. } => Self::Timeout { deadline },
            Op::OpenAt2 { dir: d, path, how } => Self::Open {
                dir: dir(d),
                path: owned_path(path),
                how: *how,
            },
            Op::Statx {
                dir: d,
                path,
                flags,
                out,
                ..
            } => Self::Statx {
                dir: dir(d),
                path: owned_path(path),
                flags,
                out,
            },
            Op::Fsync { fd, .. } => Self::Fsync { fd: fd.clone() },
            Op::RenameAt {
                old_dir,
                old_path,
                new_dir,
                new_path,
                flags,
            } => Self::Rename {
                old_dir: dir(old_dir),
                old_path: owned_path(old_path),
                new_dir: dir(new_dir),
                new_path: owned_path(new_path),
                flags,
            },
            Op::UnlinkAt {
                dir: d,
                path,
                flags,
            } => Self::Unlink {
                dir: dir(d),
                path: owned_path(path),
                flags,
            },
            Op::MkDirAt { dir: d, path, mode } => Self::Mkdir {
                dir: dir(d),
                path: owned_path(path),
                mode,
            },
            _ => return Err(PushError::Unsupported),
        })
    }

    /// Paths whose crash should fail this operation with `EIO`.
    fn disk_paths(&self, world: &World, state: &State) -> Vec<PathBuf> {
        let id = |fd: &SimFd| world.id_of(fd).ok();
        let dir = |fd: &Option<SimFd>| fd.as_ref().and_then(id);
        let at = |d: &Option<SimFd>, path: &Path| state.at_path(dir(d), path, false).ok();
        match self {
            Self::Read { fd, .. }
            | Self::Write { fd, .. }
            | Self::Writev { fd, .. }
            | Self::Fsync { fd } => id(fd)
                .and_then(|id| state.watched_path(id))
                .into_iter()
                .collect(),
            Self::Open { dir: d, path, how } => state
                .open_path(dir(d), path, how.resolve, how.flags as i32)
                .ok()
                .into_iter()
                .collect(),
            Self::Statx { dir: d, path, .. } if path.as_os_str().is_empty() => d
                .as_ref()
                .and_then(id)
                .and_then(|id| state.watched_path(id))
                .into_iter()
                .collect(),
            Self::Statx { dir: d, path, .. }
            | Self::Unlink { dir: d, path, .. }
            | Self::Mkdir { dir: d, path, .. } => at(d, path).into_iter().collect(),
            Self::Rename {
                old_dir,
                old_path,
                new_dir,
                new_path,
                ..
            } => [at(old_dir, old_path), at(new_dir, new_path)]
                .into_iter()
                .flatten()
                .collect(),
            _ => Vec::new(),
        }
    }

    /// Whether `EAGAIN` means "not ready yet, retry next turn".
    fn waits(&self) -> bool {
        match self {
            Self::Recv { flags, .. } | Self::Send { flags, .. } => flags & libc::MSG_DONTWAIT == 0,
            Self::Read { .. }
            | Self::Write { .. }
            | Self::Writev { .. }
            | Self::Poll { .. }
            | Self::Accept { .. } => true,
            Self::Connect { .. } | Self::Timeout { .. } => true,
            _ => false,
        }
    }

    /// Execute once. `limit` caps byte transfers.
    fn execute(&self, world: &World, clock: &SimClock, limit: usize) -> Result<i32> {
        let mut state = world.state();
        let id = |fd: &SimFd| world.id_of(fd);
        let dir = |fd: &Option<SimFd>| fd.as_ref().map(id).transpose();
        let count = |n: usize| i32::try_from(n).map_err(|_| errno(libc::EOVERFLOW));
        let limit = limit.min(i32::MAX as usize);
        match self {
            Self::Nop => Ok(0),
            Self::Read {
                fd,
                offset,
                buf,
                len,
            } => {
                let id = id(fd)?;
                // SAFETY: `push`'s contract keeps `buf` valid and exclusive.
                let buf = unsafe { slice_mut(*buf, *len) }?;
                count(match state.resources.get(&id) {
                    Some(Resource::File(_)) => state.file_read(id, *offset, buf, limit)?,
                    Some(Resource::Pipe(_)) => state.pipe_read(id, buf, limit)?,
                    Some(Resource::Stream(_) | Resource::Datagram(_)) => {
                        state.recv(id, buf, limit, false)?
                    }
                    Some(Resource::Listener(_)) => return Err(errno(libc::EINVAL)),
                    None => return Err(errno(libc::EBADF)),
                })
            }
            Self::Write {
                fd,
                offset,
                buf,
                len,
            } => {
                let id = id(fd)?;
                // SAFETY: `push`'s contract keeps `buf` valid.
                let buf = unsafe { slice(*buf, *len) }?;
                count(match state.resources.get(&id) {
                    Some(Resource::File(_)) => state.file_write(id, *offset, buf, limit)?,
                    Some(Resource::Pipe(_)) => state.pipe_write(id, buf, limit)?,
                    Some(Resource::Stream(_) | Resource::Datagram(_)) => {
                        state.send(id, buf, limit)?
                    }
                    Some(Resource::Listener(_)) => return Err(errno(libc::EINVAL)),
                    None => return Err(errno(libc::EBADF)),
                })
            }
            Self::Writev {
                fd,
                offset,
                iovecs,
                count: segments,
            } => {
                let id = id(fd)?;
                if *segments > 1024 {
                    return Err(errno(libc::EINVAL));
                }
                // SAFETY: `push`'s contract keeps the iovec array valid for
                // `count` entries.
                let iovecs = unsafe { iovec_slice(*iovecs, *segments) }?;
                let mut gathered = Vec::new();
                for iov in iovecs {
                    // SAFETY: `push`'s contract keeps each segment valid.
                    gathered.extend_from_slice(unsafe {
                        slice(iov.iov_base.cast::<u8>().cast_const(), iov.iov_len)
                    }?);
                }
                let buf = gathered.as_slice();
                count(match state.resources.get(&id) {
                    Some(Resource::File(_)) => state.file_write(id, *offset, buf, limit)?,
                    Some(Resource::Pipe(_)) => state.pipe_write(id, buf, limit)?,
                    Some(Resource::Stream(_) | Resource::Datagram(_)) => {
                        state.send(id, buf, limit)?
                    }
                    Some(Resource::Listener(_)) => return Err(errno(libc::EINVAL)),
                    None => return Err(errno(libc::EBADF)),
                })
            }
            Self::Recv {
                fd,
                buf,
                len,
                flags,
            } => {
                let id = id(fd)?;
                if state.is_pipe(id) || state.file(id).is_ok() {
                    return Err(errno(libc::ENOTSOCK));
                }
                // SAFETY: `push`'s contract keeps `buf` valid and exclusive.
                let buf = unsafe { slice_mut(*buf, *len) }?;
                count(state.recv(id, buf, limit, flags & libc::MSG_PEEK != 0)?)
            }
            Self::Send { fd, buf, len, .. } => {
                let id = id(fd)?;
                if state.is_pipe(id) || state.file(id).is_ok() {
                    return Err(errno(libc::ENOTSOCK));
                }
                // SAFETY: `push`'s contract keeps `buf` valid.
                let buf = unsafe { slice(*buf, *len) }?;
                count(state.send(id, buf, limit)?)
            }
            Self::Poll { fd, events } => match state.ready(id(fd)?, *events)? {
                0 => Err(errno(libc::EAGAIN)),
                ready => Ok(ready as i32),
            },
            Self::Accept { fd, peer } => {
                let accepted = state.accept(id(fd)?)?;
                state.unowned.insert(accepted);
                if !peer.is_null() {
                    let address = state.peer_addr(accepted)?;
                    // SAFETY: `push`'s contract keeps `peer` valid and exclusive.
                    unsafe { write_address(&mut **peer, &address) };
                }
                count(accepted as usize)
            }
            Self::Connect { fd, address } => {
                let address = address.clone()?;
                state.connect(id(fd)?, &address).map(|()| 0)
            }
            Self::Timeout { deadline } => {
                if clock.now() >= *deadline {
                    Err(errno(libc::ETIME))
                } else {
                    Err(errno(libc::EAGAIN))
                }
            }
            Self::Open { dir: d, path, how } => {
                let opened = state.open(dir(d)?, path, how)?;
                state.unowned.insert(opened);
                count(opened as usize)
            }
            Self::Statx {
                dir: d,
                path,
                flags,
                out,
            } => {
                let stat = state.statx(dir(d)?, path, *flags)?;
                if out.is_null() {
                    return Err(errno(libc::EFAULT));
                }
                // SAFETY: `push`'s contract keeps `out` valid and exclusive.
                unsafe { out.write(stat) };
                Ok(0)
            }
            Self::Fsync { fd } => state.sync_file(id(fd)?).map(|()| 0),
            Self::Rename {
                old_dir,
                old_path,
                new_dir,
                new_path,
                flags,
            } => {
                let from = state.at_path(dir(old_dir)?, old_path, false)?;
                let to = state.at_path(dir(new_dir)?, new_path, false)?;
                state.rename(&from, &to, *flags).map(|()| 0)
            }
            Self::Unlink {
                dir: d,
                path,
                flags,
            } => {
                let path = state.at_path(dir(d)?, path, false)?;
                state
                    .unlink(&path, flags & libc::AT_REMOVEDIR != 0)
                    .map(|()| 0)
            }
            Self::Mkdir { dir: d, path, mode } => {
                let path = state.at_path(dir(d)?, path, false)?;
                state.mkdir(&path, *mode).map(|()| 0)
            }
        }
    }
}

/// Store `address` into a reactor-owned `SockAddr` output, truncating like
/// the kernel does when it does not fit.
fn write_address(out: &mut SockAddr, address: &SocketAddress) {
    let (source, len): (Vec<u8>, usize) = match SockAddr::encode(address) {
        Ok(encoded) => {
            let len = encoded.len() as usize;
            // SAFETY: `encoded` holds `len` initialized bytes at `as_ptr`.
            let bytes = unsafe { std::slice::from_raw_parts(encoded.as_ptr().cast::<u8>(), len) };
            (bytes.to_vec(), len)
        }
        // An unnamed Unix address is just the family.
        Err(_) => {
            let family = (libc::AF_UNIX as libc::sa_family_t).to_ne_bytes();
            (family.to_vec(), family.len())
        }
    };
    let capacity = out.len() as usize;
    let copied = len.min(capacity);
    // SAFETY: `out` has room for `capacity` bytes and `source` has `len`.
    unsafe {
        std::ptr::copy_nonoverlapping(source.as_ptr(), out.as_mut_ptr().cast::<u8>(), copied)
    };
    out.set_len(len as libc::socklen_t);
}

impl Pending {
    /// Run one turn. Returns the completion result once it is due.
    fn advance(&mut self, world: &World, clock: &SimClock) -> Option<i32> {
        if let Some(result) = self.result {
            if self.hold != 0 {
                self.hold -= 1;
                return None;
            }
            return Some(result);
        }
        if self.delay != 0 {
            self.delay -= 1;
            return None;
        }
        let result = match self.error.take() {
            Some(n) => Err(errno(n)),
            None if self.crash.crashed() => Err(errno(libc::EIO)),
            None => {
                let result = self.op.execute(world, clock, self.limit);
                if self.op.waits()
                    && result.as_ref().err().and_then(Error::raw_os_error) == Some(libc::EAGAIN)
                {
                    return None;
                }
                result
            }
        };
        let result = result_of(result);
        world.state().record(
            EventKind::Complete(self.kind),
            self.user_data,
            i64::from(result),
        );
        if self.hold != 0 {
            self.result = Some(result);
            None
        } else {
            Some(result)
        }
    }
}

impl SimBackend {
    /// Create a backend over `world` with its own clock and entropy handles.
    /// Backends meant to share time should be given clones of one clock.
    pub fn new(world: &World, clock: SimClock, entropy: SimEntropy) -> Self {
        Self {
            world: world.clone(),
            clock,
            entropy,
            endpoint: None,
            capacity: usize::MAX,
            auto_advance: false,
            next_sequence: 0,
            pending: BTreeMap::new(),
            completed: VecDeque::new(),
        }
    }

    /// The world this backend operates on.
    pub fn world(&self) -> &World {
        &self.world
    }

    /// The endpoint label given to sockets this backend creates; see
    /// [partitions](crate::net#partitions).
    pub fn endpoint(&self) -> Option<&SocketAddress> {
        self.endpoint.as_ref()
    }

    /// Label sockets created from now on through this backend with
    /// `endpoint` (usually the simulated node's listening address).
    /// Existing sockets keep their labels.
    pub fn set_endpoint(&mut self, endpoint: Option<SocketAddress>) {
        self.endpoint = endpoint;
    }

    /// Whether [`wait`](Backend::wait) advances the clock to the next timer
    /// deadline when idle (default `false`).
    pub fn auto_advance(&self) -> bool {
        self.auto_advance
    }

    /// Enable or disable advancing the clock in [`wait`](Backend::wait); see
    /// the [module docs](self).
    pub fn set_auto_advance(&mut self, enabled: bool) {
        self.auto_advance = enabled;
    }

    /// Take ownership of a resource returned by an `Accept` or `OpenAt2`
    /// completion, failing with `EBADF` if `result` is not an unadopted
    /// resource; see [`World::adopt`]. [`Backend::adopt_fd`] is the
    /// infallible form the reactor uses.
    pub fn try_adopt(&self, result: i32) -> Result<SimFd> {
        self.world.adopt(result)
    }

    /// Create a listener labeled with this backend's endpoint.
    pub fn listen_on(&self, address: impl Into<SocketAddress>) -> Result<SimFd> {
        self.world
            .labeled_listen(&address.into(), self.endpoint.as_ref())
    }

    /// Synchronously connect a stream labeled with this backend's endpoint
    /// (`EAGAIN` instead of waiting when partitioned).
    pub fn connect_to(&self, address: impl Into<SocketAddress>) -> Result<SimFd> {
        self.world
            .labeled_connect(&address.into(), self.endpoint.as_ref())
    }

    /// The number of operations pushed but not yet completed (including
    /// executed operations whose completion is held).
    pub fn in_flight(&self) -> usize {
        self.pending.len()
    }

    /// A copy of the completions waiting to be popped, in order.
    pub fn queued_completions(&self) -> Vec<CompletionEntry> {
        self.completed.iter().copied().collect()
    }

    /// Test hook: queue an arbitrary completion. If an operation with this
    /// `user_data` is in flight, it is retired first (its memory is no
    /// longer touched) without producing its own completion.
    ///
    /// Fails with `ENOSPC` when [`MAX_INJECTED`] completions are queued.
    pub fn inject_completion(&mut self, user_data: u64, result: i32) -> Result<()> {
        if self.completed.len() >= MAX_INJECTED {
            return Err(errno(libc::ENOSPC));
        }
        let retired = self.take_pending(user_data, |_| true);
        self.completed.push_back(CompletionEntry {
            user_data,
            result,
            flags: 0,
        });
        drop(retired);
        Ok(())
    }

    /// Test hook: swap two queued completions to exercise arbitrary
    /// completion orders. Fails with `EINVAL` for an index out of range.
    pub fn reorder_completions(&mut self, first: usize, second: usize) -> Result<()> {
        if first >= self.completed.len() || second >= self.completed.len() {
            return Err(errno(libc::EINVAL));
        }
        self.completed.swap(first, second);
        Ok(())
    }

    fn take_pending(
        &mut self,
        user_data: u64,
        accept: impl Fn(&Pending) -> bool,
    ) -> Option<Pending> {
        let key = self
            .pending
            .iter()
            .find(|(_, p)| p.user_data == user_data && accept(p))
            .map(|(key, _)| *key)?;
        self.pending.remove(&key)
    }

    /// Advance every submitted operation by one turn.
    fn turn(&mut self, timers_only: bool) {
        let mut done = Vec::new();
        for (key, pending) in self.pending.iter_mut() {
            if !pending.submitted || timers_only && pending.kind != OpKind::Timeout {
                continue;
            }
            if let Some(result) = pending.advance(&self.world, &self.clock) {
                done.push((*key, result));
            }
        }
        // Drop retired handles only after every operation had its turn.
        let mut retired = Vec::with_capacity(done.len());
        for (key, result) in done {
            let pending = self.pending.remove(&key).expect("present");
            self.completed.push_back(CompletionEntry {
                user_data: pending.user_data,
                result,
                flags: 0,
            });
            retired.push(pending);
        }
        drop(retired);
    }

    fn next_deadline(&self) -> Option<Instant> {
        self.pending
            .values()
            .filter_map(|pending| match pending.op {
                Owned::Timeout { deadline } if pending.submitted && pending.result.is_none() => {
                    Some(deadline)
                }
                _ => None,
            })
            .min()
    }
}

impl Backend for SimBackend {
    type Fd = SimFd;
    type Clock = SimClock;
    type Entropy = SimEntropy;

    fn clock(&self) -> &SimClock {
        &self.clock
    }

    fn entropy(&self) -> &SimEntropy {
        &self.entropy
    }

    /// Record `config.capacity` as the maximum number of pushed but not yet
    /// submitted operations (the submission queue size).
    fn init(&mut self, config: &BackendConfig) -> Result<()> {
        if config.capacity == 0 {
            return Err(Error::new(uring_runtime::ErrorKind::InvalidConfiguration));
        }
        self.capacity = config.capacity as usize;
        Ok(())
    }

    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, SimFd>) -> Result<(), PushError> {
        let owned = Owned::from_op(op)?;
        let kind = op.kind();
        if self.pending.values().filter(|p| !p.submitted).count() >= self.capacity {
            return Err(PushError::Full);
        }
        let mut state = self.world.state();
        if state.knobs.reject_submissions != 0 {
            state.knobs.reject_submissions -= 1;
            state.record(EventKind::Reject(kind), user_data, 0);
            return Err(PushError::Full);
        }
        let fault = state.fault(FaultTarget::Op(kind));
        let paths = owned.disk_paths(&self.world, &state);
        let crash = state.fs.durable.watch(paths);
        state.record(EventKind::Submit(kind), user_data, 0);
        drop(state);
        let mut pending = Pending {
            user_data,
            kind,
            op: owned,
            submitted: false,
            delay: 0,
            limit: usize::MAX,
            error: None,
            hold: 0,
            result: None,
            crash,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            Some(Fault::HoldCompletion(n)) => pending.hold = n,
            None => {}
        }
        self.pending.insert(self.next_sequence, pending);
        self.next_sequence += 1;
        Ok(())
    }

    /// Adopt `raw` as [`try_adopt`](SimBackend::try_adopt) does. A `raw`
    /// that does not name an unadopted resource (for example a fabricated
    /// [`inject_completion`](SimBackend::inject_completion) result) yields a
    /// handle to an id that is never allocated: every operation on it fails
    /// with `EBADF` and dropping it closes nothing.
    unsafe fn adopt_fd(&mut self, raw: i32) -> SimFd {
        self.world
            .adopt(raw)
            .unwrap_or_else(|_| self.world.handle(u64::MAX))
    }

    /// Cancel an operation that has not executed yet. A removed target
    /// completes with `-ECANCELED` and the request with 0 (ordered by
    /// [`World::set_cancel_first`]); otherwise the request completes with
    /// `-ENOENT`. Takes effect immediately.
    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError> {
        let mut state = self.world.state();
        if let Some(Fault::Errno(n)) = state.fault(FaultTarget::Op(OpKind::Cancel)) {
            state.record(EventKind::Cancel, target, 0);
            drop(state);
            self.completed.push_back(CompletionEntry {
                user_data,
                result: -n,
                flags: 0,
            });
            return Ok(());
        }
        let cancel_first = state.knobs.cancel_first;
        drop(state);
        let removed = self.take_pending(target, |p| p.result.is_none());
        let cancel = CompletionEntry {
            user_data,
            result: if removed.is_some() { 0 } else { -libc::ENOENT },
            flags: 0,
        };
        match &removed {
            Some(pending) => {
                let canceled = CompletionEntry {
                    user_data: pending.user_data,
                    result: -libc::ECANCELED,
                    flags: 0,
                };
                if cancel_first {
                    self.completed.extend([cancel, canceled]);
                } else {
                    self.completed.extend([canceled, cancel]);
                }
            }
            None => self.completed.push_back(cancel),
        }
        self.world
            .state()
            .record(EventKind::Cancel, target, i64::from(removed.is_some()));
        drop(removed);
        Ok(())
    }

    /// Mark every pushed operation submitted and run one turn.
    fn submit(&mut self) -> Result<usize> {
        let mut submitted = 0;
        for pending in self.pending.values_mut().filter(|p| !p.submitted) {
            pending.submitted = true;
            submitted += 1;
        }
        self.turn(false);
        Ok(submitted)
    }

    fn pop(&mut self) -> Option<CompletionEntry> {
        if self.completed.is_empty() {
            self.turn(true);
        }
        self.completed.pop_front()
    }

    /// Run one turn without sleeping; see the [module docs](self) for
    /// [auto-advance](SimBackend::set_auto_advance).
    fn wait(&mut self, timeout: Duration) -> Result<()> {
        self.turn(false);
        if self.completed.is_empty() && self.auto_advance {
            let now = self.clock.now();
            let step = self.next_deadline().map_or(timeout, |deadline| {
                deadline.saturating_duration_since(now).min(timeout)
            });
            self.clock.checked_advance(step)?;
            self.turn(false);
        }
        Ok(())
    }

    /// A waker that does nothing: `wait` never blocks.
    fn remote_waker(&self) -> Result<RemoteWaker> {
        Ok(RemoteWaker::new(|| {}))
    }

    fn supports(&self, kind: OpKind) -> bool {
        OpKind::ALL.contains(&kind)
    }
}

impl Host for SimBackend {
    /// Create a socket labeled with this backend's endpoint. The
    /// `SOCK_NONBLOCK`/`SOCK_CLOEXEC` bits are accepted and ignored.
    fn socket(&self, domain: i32, ty: i32, _protocol: i32) -> Result<SimFd> {
        self.world
            .labeled_socket(domain, ty, self.endpoint.as_ref())
    }

    fn bind(&self, fd: &SimFd, address: &SocketAddress) -> Result<()> {
        let id = self.world.id_of(fd)?;
        let mut state = self.world.state();
        state.sync_limit(SyncOp::Bind)?;
        state.bind(id, address)
    }

    fn listen(&self, fd: &SimFd, backlog: i32) -> Result<()> {
        let id = self.world.id_of(fd)?;
        let mut state = self.world.state();
        state.sync_limit(SyncOp::Listen)?;
        state.listen(id, backlog)
    }

    fn local_addr(&self, fd: &SimFd) -> Result<SocketAddress> {
        self.world.state().local_addr(self.world.id_of(fd)?)
    }

    fn peer_addr(&self, fd: &SimFd) -> Result<SocketAddress> {
        self.world.state().peer_addr(self.world.id_of(fd)?)
    }

    fn shutdown(&self, fd: &SimFd, how: Shutdown) -> Result<()> {
        self.world.state().shutdown(self.world.id_of(fd)?, how)
    }

    fn set_nodelay(&self, fd: &SimFd, _enabled: bool) -> Result<()> {
        self.world.state().set_nodelay(self.world.id_of(fd)?)
    }

    fn try_send(&self, fd: &SimFd, buf: &[u8], _flags: i32) -> Result<usize> {
        self.world.id_of(fd)?;
        fd.send(buf)
    }

    fn try_recv(&self, fd: &SimFd, buf: &mut [u8], flags: i32) -> Result<usize> {
        self.world.id_of(fd)?;
        fd.recv(buf, flags & libc::MSG_PEEK != 0)
    }

    /// Like `poll(2)`: an `O_PATH` file reports `POLLNVAL`.
    fn poll_now(&self, fd: &SimFd, events: i16) -> Result<i16> {
        let id = self.world.id_of(fd)?;
        let state = self.world.state();
        match state.ready(id, events as u16 as u32) {
            Ok(ready) => Ok(ready as u16 as i16),
            Err(error)
                if error.raw_os_error() == Some(libc::EBADF)
                    && state.resources.contains_key(&id) =>
            {
                Ok(libc::POLLNVAL)
            }
            Err(error) => Err(error),
        }
    }

    /// A pipe with [`DEFAULT_PIPE_CAPACITY`] bytes of buffer.
    fn pipe(&self) -> Result<(SimFd, SimFd)> {
        self.world.pipe(DEFAULT_PIPE_CAPACITY)
    }
}

impl fmt::Debug for SimBackend {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SimBackend")
            .field("endpoint", &self.endpoint)
            .field("in_flight", &self.pending.len())
            .field("completed", &self.completed.len())
            .field("auto_advance", &self.auto_advance)
            .finish_non_exhaustive()
    }
}
