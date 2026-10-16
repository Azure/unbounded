//! The io_uring [`Backend`].
//!
//! [`Uring`] is the production backend: it owns an io_uring instance and an
//! eventfd used for cross-thread wakes, and implements [`Host`] with plain
//! nonblocking syscalls.
//!
//! # Ring setup
//!
//! [`Backend::init`] creates a ring with `capacity` submission entries and
//! twice as many completion entries. It first asks for
//! `IORING_SETUP_SUBMIT_ALL | IORING_SETUP_SINGLE_ISSUER` and falls back to
//! fewer flags when the kernel rejects them with `EINVAL`. The ring must
//! report `IORING_FEAT_NODROP` (Linux 5.5), otherwise initialization fails
//! with [`ErrorKind::Unsupported`]: the reactor's fences depend on every
//! completion being delivered. Supported opcodes are probed with
//! `IORING_REGISTER_PROBE` and reported by [`Backend::supports`].
//!
//! Because of `SINGLE_ISSUER`, a `Uring` is `!Send`: create it on the
//! thread that drives it.
//!
//! # Submitting and waiting
//!
//! [`Backend::submit`] skips the `io_uring_enter` syscall when the
//! submission queue is empty and the kernel reports neither a completion
//! queue overflow nor pending task work. Transient submit errors (`EINTR`,
//! `EAGAIN`, `EBUSY`) are reported as zero submissions; queued entries stay
//! queued for the next attempt.
//!
//! [`Backend::wait`] does not submit (the reactor submits first). It returns
//! immediately if completions are ready or a same-thread wake is pending,
//! and otherwise blocks in `ppoll(2)` on the ring descriptor and the
//! eventfd. `ppoll` rather than `io_uring_enter(GETEVENTS)` lets one call
//! wait for both completions and remote wakes without registering the
//! eventfd with the ring.
//!
//! # Remote wakes
//!
//! The [`RemoteWaker`] writes the eventfd. Wakes are coalesced: once a wake
//! is pending, further wakes skip the syscall until `wait` consumes it.
//! Wakes from the thread that initialized the ring only set a flag that
//! makes the next `wait` return immediately.
//!
//! # Timeouts
//!
//! [`Op::Timeout`] uses an absolute `CLOCK_MONOTONIC` timeout
//! (`IORING_TIMEOUT_ABS`). `std::time::Instant` is opaque, so the deadline
//! is converted by reading `Instant::now()` and
//! `clock_gettime(CLOCK_MONOTONIC)` back to back and applying the
//! difference; on Linux both read the same clock, so the error is the time
//! between the two reads.

use std::marker::PhantomData;
use std::net::Shutdown;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread::ThreadId;
use std::time::{Duration, Instant};

use io_uring::{IoUring, Probe, opcode, squeue, types};

use crate::backend::{
    BackendConfig, CompletionEntry, KernelTimespec, Op, OpKind, PushError, RemoteWaker,
};
use crate::{
    Backend, Error, ErrorKind, Fd, Host, Result, SockAddr, SocketAddress, SystemClock,
    SystemEntropy,
};

/// The io_uring backend. See the [module documentation](self).
///
/// ```
/// use uring_runtime::Backend;
/// use uring_runtime::backend::{BackendConfig, OpKind};
/// use uring_runtime::uring::Uring;
///
/// if Uring::is_available() {
///     let mut uring = Uring::new();
///     uring.init(&BackendConfig::new(64)).unwrap();
///     assert!(uring.supports(OpKind::Read));
/// }
/// ```
pub struct Uring {
    ring: Option<Ring>,
    _not_send: PhantomData<*const ()>,
}

struct Ring {
    ring: IoUring,
    probe: Probe,
    wake: Arc<WakeState>,
    single_issuer: bool,
    submit_all: bool,
}

struct WakeState {
    eventfd: OwnedFd,
    owner: ThreadId,
    /// A same-thread wake is pending.
    local: AtomicBool,
    /// An eventfd write is pending (coalescing).
    remote: AtomicBool,
}

impl WakeState {
    fn wake(&self) {
        if std::thread::current().id() == self.owner {
            self.local.store(true, Ordering::Release);
            return;
        }
        if self.remote.swap(true, Ordering::AcqRel) {
            return;
        }
        let value = 1u64;
        loop {
            // SAFETY: writes 8 bytes from a live local to an owned eventfd.
            let written = unsafe {
                libc::write(
                    self.eventfd.as_raw_fd(),
                    (&raw const value).cast(),
                    size_of::<u64>(),
                )
            };
            if written >= 0 {
                return;
            }
            match Error::last_os_error().raw_os_error() {
                Some(libc::EINTR) => continue,
                // The counter is saturated, so the eventfd is readable.
                Some(libc::EAGAIN) => return,
                _ => {
                    self.remote.store(false, Ordering::Release);
                    return;
                }
            }
        }
    }

    /// Consume a pending eventfd wake.
    fn drain(&self) {
        let mut value = 0u64;
        // SAFETY: reads 8 bytes into a live local from an owned, nonblocking
        // eventfd. Failure (EAGAIN) means there was nothing to consume.
        unsafe {
            libc::read(
                self.eventfd.as_raw_fd(),
                (&raw mut value).cast(),
                size_of::<u64>(),
            );
        }
        // Clear after reading, never before: a producer that raced with the
        // read is covered by the reactor turn that follows this wait, and a
        // later producer writes a fresh token.
        self.remote.store(false, Ordering::Release);
    }
}

impl Default for Uring {
    fn default() -> Self {
        Self::new()
    }
}

impl std::fmt::Debug for Uring {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let mut debug = f.debug_struct("Uring");
        if let Some(ring) = &self.ring {
            debug
                .field("sq_entries", &ring.ring.params().sq_entries())
                .field("cq_entries", &ring.ring.params().cq_entries())
                .field("single_issuer", &ring.single_issuer)
                .field("submit_all", &ring.submit_all);
        }
        debug.finish_non_exhaustive()
    }
}

fn unavailable() -> Error {
    Error::new(ErrorKind::Unavailable).with_context("io_uring backend not initialized")
}

fn cvt(result: libc::c_int, context: &'static str) -> Result<libc::c_int> {
    if result < 0 {
        Err(Error::last_os_error().with_context(context))
    } else {
        Ok(result)
    }
}

fn cvt_size(result: isize) -> Result<usize> {
    usize::try_from(result).map_err(|_| Error::last_os_error())
}

/// `None` means `AT_FDCWD`.
fn dir_fd(dir: Option<&Fd>) -> types::Fd {
    types::Fd(dir.map_or(libc::AT_FDCWD, AsRawFd::as_raw_fd))
}

fn fd(fd: &Fd) -> types::Fd {
    types::Fd(fd.as_raw_fd())
}

/// Convert `deadline` to an absolute `CLOCK_MONOTONIC` time.
fn monotonic_timespec(deadline: Instant) -> KernelTimespec {
    let now = Instant::now();
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // SAFETY: `ts` is a valid, writable timespec. CLOCK_MONOTONIC cannot fail.
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    let base = Duration::new(ts.tv_sec as u64, ts.tv_nsec as u32);
    let target = if deadline >= now {
        base.saturating_add(deadline - now)
    } else {
        base.saturating_sub(now - deadline)
    };
    KernelTimespec {
        tv_sec: i64::try_from(target.as_secs()).unwrap_or(i64::MAX),
        tv_nsec: i64::from(target.subsec_nanos()),
    }
}

impl Uring {
    /// Create an uninitialized backend. The ring is created by
    /// [`Backend::init`] (normally through
    /// [`Reactor::new`](crate::reactor::Reactor::new)).
    pub fn new() -> Self {
        Self {
            ring: None,
            _not_send: PhantomData,
        }
    }

    /// Whether this process can create an io_uring instance (the kernel
    /// supports it and neither seccomp nor `kernel.io_uring_disabled`
    /// forbids it).
    pub fn is_available() -> bool {
        IoUring::new(2).is_ok()
    }

    /// Whether the ring was created with `IORING_SETUP_SINGLE_ISSUER`.
    pub fn single_issuer(&self) -> bool {
        self.ring.as_ref().is_some_and(|ring| ring.single_issuer)
    }

    /// Whether the ring was created with `IORING_SETUP_SUBMIT_ALL`.
    pub fn submit_all(&self) -> bool {
        self.ring.as_ref().is_some_and(|ring| ring.submit_all)
    }

    fn ring(&mut self) -> Result<&mut Ring> {
        self.ring.as_mut().ok_or_else(unavailable)
    }
}

/// Create a ring, dropping optional setup flags the kernel rejects.
fn build_ring(sq: u32, cq: u32) -> Result<(IoUring, bool, bool)> {
    const ATTEMPTS: [(bool, bool); 3] = [(true, true), (true, false), (false, false)];
    for (index, (submit_all, single_issuer)) in ATTEMPTS.into_iter().enumerate() {
        let mut builder = IoUring::builder();
        builder.setup_cqsize(cq);
        if submit_all {
            builder.setup_submit_all();
        }
        if single_issuer {
            builder.setup_single_issuer();
        }
        match builder.build(sq) {
            Ok(ring) => return Ok((ring, submit_all, single_issuer)),
            Err(error)
                if error.raw_os_error() == Some(libc::EINVAL) && index + 1 < ATTEMPTS.len() => {}
            Err(error) => return Err(Error::from(error).with_context("io_uring_setup")),
        }
    }
    unreachable!("the last attempt returns")
}

impl Backend for Uring {
    type Fd = Fd;
    type Clock = SystemClock;
    type Entropy = SystemEntropy;

    fn clock(&self) -> &SystemClock {
        &SystemClock
    }

    fn entropy(&self) -> &SystemEntropy {
        &SystemEntropy
    }

    fn init(&mut self, config: &BackendConfig) -> Result<()> {
        if self.ring.is_some() {
            return Err(Error::new(ErrorKind::InvalidConfiguration)
                .with_context("io_uring backend already initialized"));
        }
        let sq = config.capacity.max(1);
        let cq = sq.checked_mul(2).ok_or_else(|| {
            Error::new(ErrorKind::InvalidConfiguration).with_context("io_uring capacity too large")
        })?;
        let (ring, submit_all, single_issuer) = build_ring(sq, cq)?;
        if !ring.params().is_feature_nodrop() {
            return Err(Error::new(ErrorKind::Unsupported)
                .with_context("io_uring lacks IORING_FEAT_NODROP"));
        }
        let mut probe = Probe::new();
        ring.submitter()
            .register_probe(&mut probe)
            .map_err(|error| Error::from(error).with_context("io_uring probe"))?;
        // SAFETY: no pointers are involved; the result is checked below.
        let eventfd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        let eventfd = cvt(eventfd, "eventfd")?;
        self.ring = Some(Ring {
            ring,
            probe,
            wake: Arc::new(WakeState {
                // SAFETY: `eventfd` is a fresh descriptor that nothing else owns.
                eventfd: unsafe { OwnedFd::from_raw_fd(eventfd) },
                owner: std::thread::current().id(),
                local: AtomicBool::new(false),
                remote: AtomicBool::new(false),
            }),
            single_issuer,
            submit_all,
        });
        Ok(())
    }

    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, Fd>) -> Result<(), PushError> {
        let ring = self.ring()?;
        let entry: squeue::Entry = match *op {
            Op::Nop => opcode::Nop::new().build(),
            Op::Read {
                fd: file,
                offset,
                buf,
                len,
            } => opcode::Read::new(fd(file), buf, len).offset(offset).build(),
            Op::Write {
                fd: file,
                offset,
                buf,
                len,
            } => opcode::Write::new(fd(file), buf, len)
                .offset(offset)
                .build(),
            Op::Recv {
                fd: socket,
                buf,
                len,
                flags,
            } => opcode::Recv::new(fd(socket), buf, len).flags(flags).build(),
            Op::Send {
                fd: socket,
                buf,
                len,
                flags,
            } => opcode::Send::new(fd(socket), buf, len).flags(flags).build(),
            Op::PollAdd { fd: file, events } => opcode::PollAdd::new(fd(file), events).build(),
            Op::Accept {
                fd: socket,
                peer,
                flags,
            } => {
                let (address, len) = if peer.is_null() {
                    (std::ptr::null_mut(), std::ptr::null_mut())
                } else {
                    // SAFETY: the caller guarantees `peer` is valid and
                    // unaliased until the completion is popped.
                    let peer = unsafe { &mut *peer };
                    peer.set_len(size_of::<libc::sockaddr_storage>() as libc::socklen_t);
                    (peer.as_mut_ptr(), peer.len_mut_ptr())
                };
                opcode::Accept::new(fd(socket), address, len)
                    .flags(flags)
                    .build()
            }
            Op::Connect {
                fd: socket,
                address,
            } => opcode::Connect::new(fd(socket), address.as_ptr(), address.len()).build(),
            Op::Timeout { deadline, timespec } => {
                // SAFETY: the caller guarantees `timespec` is valid and
                // unaliased until the completion is popped.
                unsafe { timespec.write(monotonic_timespec(deadline)) };
                // `types::Timespec` is a transparent `__kernel_timespec`,
                // which `KernelTimespec` mirrors with `repr(C)`.
                opcode::Timeout::new(timespec.cast_const().cast::<types::Timespec>())
                    .flags(types::TimeoutFlags::ABS)
                    .build()
            }
            Op::OpenAt2 { dir, path, how } => {
                // `types::OpenHow` is a transparent `open_how`, which
                // `OpenHow` mirrors with `repr(C)`.
                let how = std::ptr::from_ref(how).cast::<types::OpenHow>();
                opcode::OpenAt2::new(dir_fd(dir), path.as_ptr(), how).build()
            }
            Op::Statx {
                dir,
                path,
                flags,
                mask,
                out,
            } => opcode::Statx::new(dir_fd(dir), path.as_ptr(), out.cast::<types::statx>())
                .flags(flags)
                .mask(mask)
                .build(),
            Op::Fsync { fd: file, datasync } => {
                let flags = if datasync {
                    types::FsyncFlags::DATASYNC
                } else {
                    types::FsyncFlags::empty()
                };
                opcode::Fsync::new(fd(file)).flags(flags).build()
            }
            Op::RenameAt {
                old_dir,
                old_path,
                new_dir,
                new_path,
                flags,
            } => opcode::RenameAt::new(
                dir_fd(old_dir),
                old_path.as_ptr(),
                dir_fd(new_dir),
                new_path.as_ptr(),
            )
            .flags(flags)
            .build(),
            Op::UnlinkAt { dir, path, flags } => opcode::UnlinkAt::new(dir_fd(dir), path.as_ptr())
                .flags(flags)
                .build(),
            Op::MkDirAt { dir, path, mode } => opcode::MkDirAt::new(dir_fd(dir), path.as_ptr())
                .mode(mode as libc::mode_t)
                .build(),
        };
        if !ring.probe.is_supported(entry.get_opcode() as u8) {
            return Err(PushError::Unsupported);
        }
        let entry = entry.user_data(user_data);
        // SAFETY: the caller guarantees every pointer in `op` stays valid
        // until the completion for `user_data` is popped.
        unsafe { ring.ring.submission().push(&entry) }.map_err(|_| PushError::Full)
    }

    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError> {
        let ring = self.ring()?;
        let entry = opcode::AsyncCancel::new(target)
            .build()
            .user_data(user_data);
        // SAFETY: a cancel request references no memory.
        unsafe { ring.ring.submission().push(&entry) }.map_err(|_| PushError::Full)
    }

    unsafe fn adopt_fd(&mut self, raw: i32) -> Fd {
        // SAFETY: the caller guarantees `raw` is a descriptor the kernel just
        // created for us and that nothing else owns it.
        Fd::from(unsafe { OwnedFd::from_raw_fd(raw) })
    }

    fn submit(&mut self) -> Result<usize> {
        let ring = &mut self.ring()?.ring;
        let required = {
            let sq = ring.submission();
            !sq.is_empty() || sq.cq_overflow() || sq.taskrun()
        };
        if !required {
            return Ok(0);
        }
        match ring.submit() {
            Ok(submitted) => Ok(submitted),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::EINTR | libc::EAGAIN | libc::EBUSY)
                ) =>
            {
                Ok(0)
            }
            Err(error) => Err(Error::from(error).with_context("io_uring_enter")),
        }
    }

    fn pop(&mut self) -> Option<CompletionEntry> {
        let entry = self.ring.as_mut()?.ring.completion().next()?;
        Some(CompletionEntry {
            user_data: entry.user_data(),
            result: entry.result(),
            flags: entry.flags(),
        })
    }

    fn wait(&mut self, timeout: Duration) -> Result<()> {
        let ring = self.ring()?;
        if timeout.is_zero()
            || ring.wake.local.swap(false, Ordering::AcqRel)
            || !ring.ring.completion().is_empty()
        {
            return Ok(());
        }
        let mut fds = [
            libc::pollfd {
                fd: ring.ring.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            },
            libc::pollfd {
                fd: ring.wake.eventfd.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            },
        ];
        let timeout = libc::timespec {
            tv_sec: timeout.as_secs().min(libc::time_t::MAX as u64) as libc::time_t,
            tv_nsec: timeout.subsec_nanos() as libc::c_long,
        };
        // SAFETY: `fds` and `timeout` are valid for the duration of the call.
        let ready = unsafe {
            libc::ppoll(
                fds.as_mut_ptr(),
                fds.len() as libc::nfds_t,
                &timeout,
                std::ptr::null(),
            )
        };
        if ready < 0 {
            let error = Error::last_os_error();
            if error.is_interrupted() {
                return Ok(());
            }
            return Err(error.with_context("ppoll"));
        }
        if fds[1].revents & libc::POLLIN != 0 {
            ring.wake.drain();
        }
        if fds
            .iter()
            .any(|fd| fd.revents & (libc::POLLERR | libc::POLLNVAL) != 0)
        {
            return Err(Error::new(ErrorKind::Os).with_context("io_uring wait descriptor failed"));
        }
        Ok(())
    }

    fn remote_waker(&self) -> Result<RemoteWaker> {
        let wake = self.ring.as_ref().ok_or_else(unavailable)?.wake.clone();
        Ok(RemoteWaker::new(move || wake.wake()))
    }

    fn supports(&self, kind: OpKind) -> bool {
        let Some(ring) = &self.ring else {
            return false;
        };
        let code = match kind {
            OpKind::Nop => opcode::Nop::CODE,
            OpKind::Read => opcode::Read::CODE,
            OpKind::Write => opcode::Write::CODE,
            OpKind::Recv => opcode::Recv::CODE,
            OpKind::Send => opcode::Send::CODE,
            OpKind::PollAdd => opcode::PollAdd::CODE,
            OpKind::Accept => opcode::Accept::CODE,
            OpKind::Connect => opcode::Connect::CODE,
            OpKind::Timeout => opcode::Timeout::CODE,
            OpKind::OpenAt2 => opcode::OpenAt2::CODE,
            OpKind::Statx => opcode::Statx::CODE,
            OpKind::Fsync => opcode::Fsync::CODE,
            OpKind::RenameAt => opcode::RenameAt::CODE,
            OpKind::UnlinkAt => opcode::UnlinkAt::CODE,
            OpKind::MkDirAt => opcode::MkDirAt::CODE,
            OpKind::Cancel => opcode::AsyncCancel::CODE,
        };
        ring.probe.is_supported(code)
    }
}

fn raw(fd: &Fd) -> RawFd {
    fd.as_raw_fd()
}

impl Host for Uring {
    fn socket(&self, domain: i32, ty: i32, protocol: i32) -> Result<Fd> {
        // SAFETY: no pointers are involved.
        let fd = unsafe {
            libc::socket(
                domain,
                ty | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                protocol,
            )
        };
        let fd = cvt(fd, "socket")?;
        // SAFETY: `fd` is a fresh descriptor that nothing else owns.
        Ok(Fd::from(unsafe { OwnedFd::from_raw_fd(fd) }))
    }

    fn bind(&self, fd: &Fd, address: &SocketAddress) -> Result<()> {
        let address = SockAddr::encode(address)?;
        // SAFETY: `address` is a valid encoded sockaddr of `len` bytes.
        cvt(
            unsafe { libc::bind(raw(fd), address.as_ptr(), address.len()) },
            "bind",
        )
        .map(drop)
    }

    fn listen(&self, fd: &Fd, backlog: i32) -> Result<()> {
        // SAFETY: no pointers are involved.
        cvt(unsafe { libc::listen(raw(fd), backlog) }, "listen").map(drop)
    }

    fn local_addr(&self, fd: &Fd) -> Result<SocketAddress> {
        let mut address = SockAddr::empty();
        // SAFETY: `address` provides storage and a length of matching size.
        let result =
            unsafe { libc::getsockname(raw(fd), address.as_mut_ptr(), address.len_mut_ptr()) };
        cvt(result, "getsockname")?;
        address.decode()
    }

    fn peer_addr(&self, fd: &Fd) -> Result<SocketAddress> {
        let mut address = SockAddr::empty();
        // SAFETY: `address` provides storage and a length of matching size.
        let result =
            unsafe { libc::getpeername(raw(fd), address.as_mut_ptr(), address.len_mut_ptr()) };
        cvt(result, "getpeername")?;
        address.decode()
    }

    fn shutdown(&self, fd: &Fd, how: Shutdown) -> Result<()> {
        let how = match how {
            Shutdown::Read => libc::SHUT_RD,
            Shutdown::Write => libc::SHUT_WR,
            Shutdown::Both => libc::SHUT_RDWR,
        };
        // SAFETY: no pointers are involved.
        cvt(unsafe { libc::shutdown(raw(fd), how) }, "shutdown").map(drop)
    }

    fn set_nodelay(&self, fd: &Fd, enabled: bool) -> Result<()> {
        let value = libc::c_int::from(enabled);
        // SAFETY: passes a pointer to a live c_int and its size.
        cvt(
            unsafe {
                libc::setsockopt(
                    raw(fd),
                    libc::IPPROTO_TCP,
                    libc::TCP_NODELAY,
                    (&raw const value).cast(),
                    size_of::<libc::c_int>() as libc::socklen_t,
                )
            },
            "setsockopt(TCP_NODELAY)",
        )
        .map(drop)
    }

    fn try_send(&self, fd: &Fd, buf: &[u8], flags: i32) -> Result<usize> {
        // SAFETY: `buf` is valid for `buf.len()` readable bytes.
        cvt_size(unsafe {
            libc::send(
                raw(fd),
                buf.as_ptr().cast(),
                buf.len(),
                flags | libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
            )
        })
    }

    fn try_recv(&self, fd: &Fd, buf: &mut [u8], flags: i32) -> Result<usize> {
        // SAFETY: `buf` is valid for `buf.len()` writable bytes.
        cvt_size(unsafe {
            libc::recv(
                raw(fd),
                buf.as_mut_ptr().cast(),
                buf.len(),
                flags | libc::MSG_DONTWAIT,
            )
        })
    }

    fn poll_now(&self, fd: &Fd, events: i16) -> Result<i16> {
        let mut pollfd = libc::pollfd {
            fd: raw(fd),
            events,
            revents: 0,
        };
        // SAFETY: `pollfd` is a valid array of one element.
        cvt(unsafe { libc::poll(&mut pollfd, 1, 0) }, "poll")?;
        Ok(pollfd.revents)
    }

    fn pipe(&self) -> Result<(Fd, Fd)> {
        let mut fds = [0; 2];
        // SAFETY: `fds` is a valid array of two descriptors.
        let result = unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) };
        cvt(result, "pipe2")?;
        // SAFETY: both descriptors are fresh and owned by nothing else.
        let (read, write) = unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };
        Ok((Fd::from(read), Fd::from(write)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[cfg_attr(miri, ignore = "makes syscalls")]
    fn monotonic_conversion_tracks_instant() {
        let now = Instant::now();
        let later = monotonic_timespec(now + Duration::from_secs(5));
        let earlier = monotonic_timespec(now);
        let delta =
            (later.tv_sec - earlier.tv_sec) * 1_000_000_000 + (later.tv_nsec - earlier.tv_nsec);
        assert!((4_900_000_000..=5_100_000_000).contains(&delta), "{delta}");
        assert!(later.tv_nsec < 1_000_000_000);
        let past = monotonic_timespec(now - Duration::from_millis(1));
        assert!(past.tv_sec >= 0);
    }

    #[test]
    #[cfg_attr(miri, ignore = "makes syscalls")]
    fn uninitialized_backend_is_unavailable() {
        let mut uring = Uring::new();
        assert!(!uring.supports(OpKind::Nop));
        assert_eq!(uring.submit().unwrap_err().kind(), ErrorKind::Unavailable);
        assert!(uring.pop().is_none());
        assert!(uring.remote_waker().is_err());
        assert!(matches!(uring.cancel(1, 2), Err(PushError::Failed(_))));
        assert!(format!("{uring:?}").contains("Uring"));
    }
}
