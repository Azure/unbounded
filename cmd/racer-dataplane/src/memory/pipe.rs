//! Bounded, nonblocking, per-reader kernel pipes.
//!
//! Each lease owns both descriptors and its admission charge. No descriptor or
//! backing page is recycled while a reader owns the lease. Writes copy into kernel
//! pipe pages before splice: socket acceptance is not a userspace page reuse fence.
use crate::runtime::reactor::Descriptor as OwnedFd;
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::Reactor,
    },
};
use std::{
    cell::RefCell,
    collections::VecDeque,
    future::poll_fn,
    io,
    os::fd::{AsFd, AsRawFd, FromRawFd},
    rc::Rc,
    task::{Poll, Waker},
};

/// Maximum kernel buffer capacity per admitted reader. Pipe admission is in pipe
/// units, so total pipe capacity is bounded by Limits::pipes * MAX_PIPE_BYTES.
/// Spliced bytes retained by sockets are subject to socket buffer limits instead.
pub const MAX_PIPE_BYTES: usize = 64 * 1024;

pub struct PipePool {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    waiting: Waiters,
}

type Waiters = Rc<RefCell<VecDeque<Rc<RefCell<Option<Waker>>>>>>;

// Contains no reactor reference: a pending send can own this through its fence.
struct Notify(Waiters);
impl Drop for Notify {
    fn drop(&mut self) {
        wake_front(&self.0);
    }
}
fn wake_front(waiters: &Waiters) {
    let wake = waiters
        .borrow()
        .front()
        .and_then(|entry| entry.borrow().clone());
    if let Some(wake) = wake {
        wake.wake();
    }
}
struct Waiting {
    queue: Waiters,
    entry: Rc<RefCell<Option<Waker>>>,
    _reservation: Reservation,
}
impl Drop for Waiting {
    fn drop(&mut self) {
        {
            let mut queue = self.queue.borrow_mut();
            queue.retain(|entry| !Rc::ptr_eq(entry, &self.entry));
            if queue.is_empty() {
                // Do not retain queue storage after its admission charges leave.
                *queue = VecDeque::new();
            }
        }
        wake_front(&self.queue);
    }
}

pub struct PipeLease {
    read: OwnedFd,
    write: OwnedFd,
    capacity: usize,
    buffered: usize,
    // Declared after the descriptors so capacity is returned only after closing.
    _reservation: Reservation,
    // Wake only after descriptors close and admission is returned.
    _notify: Notify,
}

impl PipePool {
    pub fn new(admission: Rc<Admission>, reactor: Rc<Reactor>) -> Self {
        Self {
            admission,
            reactor,
            waiting: Rc::default(),
        }
    }

    pub(crate) fn admission(&self) -> &Admission {
        &self.admission
    }

    pub(crate) fn reactor(&self) -> &Reactor {
        &self.reactor
    }

    /// FIFO scheduling above immediate raw admission. At most queue_entries wait
    /// without pipes or new page acquisitions; each entry charges context bytes
    /// for its guard, queue slot, wake cell, and cancellation registration.
    /// The owning worker's bounded tick checks deadlines and stopped admission.
    pub(crate) fn acquire_wait<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, PipeLease> {
        Box::pin(async move {
            scope.check()?;
            if self.waiting.borrow().is_empty() {
                match self.acquire() {
                    Err(Error::Overloaded) => {}
                    result => return result,
                }
            }
            if self.waiting.borrow().len() >= self.admission.limits().queue_entries.get() {
                return Err(Error::Overloaded);
            }
            let reservation = self.admission.reserve(
                None,
                ResourceClass::RequestContext,
                std::mem::size_of::<Waiting>() + 128,
            )?;
            let cancellation = scope.cancellation.subscribe()?;
            let entry = Rc::new(RefCell::new(None));
            self.waiting.borrow_mut().push_back(entry.clone());
            let waiting = Waiting {
                queue: self.waiting.clone(),
                entry,
                _reservation: reservation,
            };
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                if self.admission.is_stopped() {
                    return Poll::Ready(Err(Error::Unavailable));
                }
                *waiting.entry.borrow_mut() = Some(cx.waker().clone());
                if self
                    .waiting
                    .borrow()
                    .front()
                    .is_some_and(|entry| Rc::ptr_eq(entry, &waiting.entry))
                {
                    match self.acquire() {
                        Err(Error::Overloaded) => {}
                        result => return Poll::Ready(result),
                    }
                }
                Poll::Pending
            })
            .await
        })
    }

    /// Reserve before creating descriptors. Exhaustion never waits for a reader.
    pub fn acquire(&self) -> Result<PipeLease> {
        let reservation = self.admission.reserve(None, ResourceClass::Pipe, 1)?;
        reservation.validate(ResourceClass::Pipe, 1)?;
        #[cfg(test)]
        if let Some(sim) = crate::runtime::reactor::simulation::Simulation::current() {
            let (read, write) = sim.pipe(MAX_PIPE_BYTES);
            return Ok(PipeLease {
                read,
                write,
                capacity: MAX_PIPE_BYTES,
                buffered: 0,
                _reservation: reservation,
                _notify: Notify(self.waiting.clone()),
            });
        }
        let mut fds = [-1; 2];
        // SAFETY: pipe2 initializes exactly two descriptors on success.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) } < 0 {
            return Err(Error::Io);
        }
        // SAFETY: both descriptors were newly created and have unique owners.
        let read = unsafe { OwnedFd::from_raw_fd(fds[0]) };
        let write = unsafe { OwnedFd::from_raw_fd(fds[1]) };
        // SAFETY: fcntl operates on a live descriptor and requires no pointer.
        let mut capacity = unsafe { libc::fcntl(write.as_raw_fd(), libc::F_GETPIPE_SZ) };
        if capacity < 0 {
            return Err(Error::Io);
        }
        if capacity as usize > MAX_PIPE_BYTES {
            // SAFETY: the empty pipe can be shrunk without borrowing user memory.
            capacity = unsafe {
                libc::fcntl(write.as_raw_fd(), libc::F_SETPIPE_SZ, MAX_PIPE_BYTES as i32)
            };
        }
        if capacity <= 0 || capacity as usize > MAX_PIPE_BYTES {
            return Err(Error::Io);
        }
        Ok(PipeLease {
            read,
            write,
            capacity: capacity as usize,
            buffered: 0,
            _reservation: reservation,
            _notify: Notify(self.waiting.clone()),
        })
    }
}

impl PipeLease {
    pub fn capacity(&self) -> usize {
        self.capacity
    }

    pub fn buffered(&self) -> usize {
        self.buffered
    }

    /// Copy at most the available capacity. WouldBlock and Interrupted are exposed
    /// to the caller; this method never waits or retains a borrowed buffer.
    pub fn try_write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        #[cfg(test)]
        if let OwnedFd::Sim(handle) = &self.write {
            let written = handle.pipe_write(bytes)?;
            self.buffered += written;
            return Ok(written);
        }
        // SAFETY: the initialized slice stays live for this nonblocking syscall.
        // The read end is owned by this lease, so this cannot generate SIGPIPE.
        let written = unsafe {
            libc::write(
                self.write.as_raw_fd(),
                bytes.as_ptr().cast(),
                bytes.len().min(self.capacity),
            )
        };
        let written = syscall_count(written)?;
        self.buffered += written;
        Ok(written)
    }

    /// Read currently buffered bytes, or return WouldBlock for an empty pipe.
    pub fn try_read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        #[cfg(test)]
        if let OwnedFd::Sim(handle) = &self.read {
            let read = handle.pipe_read(bytes)?;
            self.buffered -= read;
            return Ok(read);
        }
        // SAFETY: the destination is exclusively borrowed until read returns.
        let read = unsafe {
            libc::read(
                self.read.as_raw_fd(),
                bytes.as_mut_ptr().cast(),
                bytes.len(),
            )
        };
        let read = syscall_count(read)?;
        self.buffered -= read;
        Ok(read)
    }

    pub fn try_splice_descriptor(&mut self, socket: &OwnedFd, count: usize) -> io::Result<usize> {
        #[cfg(test)]
        if let (OwnedFd::Sim(pipe), OwnedFd::Sim(socket)) = (&self.read, socket) {
            let sent = pipe.splice(socket, count.min(self.buffered))?;
            self.buffered -= sent;
            return Ok(sent);
        }
        self.try_splice_to(socket, count)
    }

    /// Transfer copied kernel pipe bytes to a nonblocking stream socket. The
    /// caller retains both owners through this synchronous syscall. Unsupported
    /// splice errors leave the bytes in the pipe for an independent copy fallback.
    pub fn try_splice_to(&mut self, socket: &impl AsFd, count: usize) -> io::Result<usize> {
        let fd = socket.as_fd().as_raw_fd();
        // SPLICE_F_NONBLOCK controls the pipe side; the socket must also be
        // nonblocking. Never risk a blocking call for an arbitrary caller's FD.
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
        if flags < 0 {
            return Err(io::Error::last_os_error());
        }
        if flags & libc::O_NONBLOCK == 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        // O_NONBLOCK does not prevent regular-file I/O from blocking. Restrict
        // this public API to stream sockets before entering splice.
        let mut kind: libc::c_int = 0;
        let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
        // SAFETY: both output pointers reference correctly sized local values.
        if unsafe {
            libc::getsockopt(
                fd,
                libc::SOL_SOCKET,
                libc::SO_TYPE,
                (&mut kind as *mut libc::c_int).cast(),
                &mut length,
            )
        } < 0
        {
            return Err(io::Error::last_os_error());
        }
        if kind != libc::SOCK_STREAM {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        if count == 0 || self.buffered == 0 {
            return Ok(0);
        }
        // Unlike send, splice has no MSG_NOSIGNAL. Mask only on this worker and
        // only across the syscall, consuming our own EPIPE signal before restore.
        let signal = SigpipeGuard::block()?;
        // SAFETY: owned live FDs, null offsets for pipe/socket, no userspace page
        // pointers, and both ends are nonblocking. No vmsplice/GIFT is involved.
        let result = syscall_count(unsafe {
            libc::splice(
                self.read.as_raw_fd(),
                std::ptr::null_mut(),
                fd,
                std::ptr::null_mut(),
                count.min(self.buffered),
                libc::SPLICE_F_NONBLOCK,
            )
        });
        if result
            .as_ref()
            .is_err_and(|e| e.raw_os_error() == Some(libc::EPIPE))
        {
            signal.consume_generated();
        }
        drop(signal);
        if let Ok(sent) = result {
            self.buffered -= sent;
        }
        result
    }
}

struct SigpipeGuard {
    previous: libc::sigset_t,
    set: libc::sigset_t,
    was_pending: bool,
}

impl SigpipeGuard {
    fn block() -> io::Result<Self> {
        // SAFETY: all signal set pointers refer to initialized local storage.
        unsafe {
            let mut guard = Self {
                previous: std::mem::zeroed(),
                set: std::mem::zeroed(),
                was_pending: true,
            };
            libc::sigemptyset(&mut guard.set);
            libc::sigaddset(&mut guard.set, libc::SIGPIPE);
            let error = libc::pthread_sigmask(libc::SIG_BLOCK, &guard.set, &mut guard.previous);
            if error != 0 {
                // No mask was installed; do not run the restoring destructor.
                std::mem::forget(guard);
                return Err(io::Error::from_raw_os_error(error));
            }
            let mut pending = std::mem::zeroed();
            if libc::sigpending(&mut pending) < 0 {
                return Err(io::Error::last_os_error());
            }
            guard.was_pending = libc::sigismember(&pending, libc::SIGPIPE) == 1;
            Ok(guard)
        }
    }

    fn consume_generated(&self) {
        if self.was_pending {
            return;
        }
        let timeout = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        // SAFETY: zero timeout never waits; only our thread's blocked SIGPIPE is
        // consumed. Preserve a signal that was pending before entering the guard.
        while unsafe { libc::sigtimedwait(&self.set, std::ptr::null_mut(), &timeout) } < 0 {
            if io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                break;
            }
        }
    }
}

impl Drop for SigpipeGuard {
    fn drop(&mut self) {
        // SAFETY: restore the exact thread mask saved by successful pthread_sigmask.
        unsafe { libc::pthread_sigmask(libc::SIG_SETMASK, &self.previous, std::ptr::null_mut()) };
    }
}

fn syscall_count(value: isize) -> io::Result<usize> {
    if value < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::model::limits::Limits;

    pub(in crate::memory) fn admission(pipes: usize) -> Rc<Admission> {
        let small = std::num::NonZeroUsize::new(8).unwrap();
        let bytes = std::num::NonZeroUsize::new(32 * 1024 * 1024).unwrap();
        Rc::new(Admission::new(Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: small,
            waiters_per_flight: small,
            queue_entries: small,
            connections_per_neighbor: small,
            client_connections: small,
            pipes: std::num::NonZeroUsize::new(pipes).unwrap(),
            range_window_pages: small,
            replay_entries: small,
            header_bytes: small,
            cached_rankings: small,
            cached_paths: small,
            retained_snapshots: small,
            metadata_entries: small,
            relay_transfers: small,
        }))
    }

    #[test]
    fn exhaustion_and_drop_return_capacity_even_after_pool_drop() {
        let admission = admission(1);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = PipePool::new(admission.clone(), reactor.clone());
        let lease = pool.acquire().unwrap();
        assert!(matches!(pool.acquire(), Err(Error::Overloaded)));
        drop(pool);
        let pool = PipePool::new(admission, reactor);
        assert!(matches!(pool.acquire(), Err(Error::Overloaded)));
        drop(lease);
        assert!(pool.acquire().is_ok());
    }

    #[test]
    fn scheduled_acquisition_is_bounded_fifo_and_wakes_only_for_progress() {
        use crate::{model::identity::RequestId, test_support::WakeCounter};
        use std::{
            sync::Arc,
            task::Context,
            time::{Duration, Instant},
        };
        let admission = admission(2);
        let pool = PipePool::new(admission.clone(), Rc::new(Reactor::new(admission.clone())));
        let held = [pool.acquire().unwrap(), pool.acquire().unwrap()];
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut waiting: Vec<_> = (0..8).map(|_| pool.acquire_wait(&scope)).collect();
        for wait in &mut waiting {
            assert!(wait.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(count.count(), 0, "waiting does not spin/self-wake");
        assert!(matches!(
            pool.acquire_wait(&scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(pool.waiting.borrow().len(), 8);
        assert_eq!(admission.used(ResourceClass::Pipe), 2);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        drop(held);
        assert!(count.count() > 0);
        // Reverse polling cannot let new arrivals jump the queue.
        for wait in waiting.iter_mut().skip(1).rev() {
            assert!(wait.as_mut().poll(&mut cx).is_pending());
        }
        let mut leases = VecDeque::new();
        for mut wait in waiting {
            if leases.len() == 2 {
                leases.pop_front();
            }
            let Poll::Ready(Ok(pipe)) = wait.as_mut().poll(&mut cx) else {
                panic!("FIFO waiter did not progress")
            };
            leases.push_back(pipe);
            assert!(admission.used(ResourceClass::Pipe) <= 2);
        }
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        drop(leases);
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
    }

    #[test]
    fn scheduled_wait_cancellation_deadline_stop_and_abandonment_release_admission() {
        use crate::{model::identity::RequestId, test_support::WakeCounter};
        use std::{
            sync::Arc,
            task::Context,
            time::{Duration, Instant},
        };
        for failure in [
            Some(Error::Cancelled),
            Some(Error::DeadlineExceeded),
            Some(Error::Unavailable),
            None,
        ] {
            let admission = admission(1);
            let pool = PipePool::new(admission.clone(), Rc::new(Reactor::new(admission.clone())));
            let held = pool.acquire().unwrap();
            let scope = RequestScope::new(
                RequestId([0; 16]),
                Instant::now()
                    + if failure == Some(Error::DeadlineExceeded) {
                        Duration::from_millis(10)
                    } else {
                        Duration::from_secs(5)
                    },
            )
            .unwrap();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut wait = pool.acquire_wait(&scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert!(admission.used(ResourceClass::RequestContext) > 0);
            match failure {
                Some(Error::Cancelled) => {
                    scope.cancel().unwrap();
                    assert!(count.count() > 0);
                }
                Some(Error::DeadlineExceeded) => std::thread::sleep(Duration::from_millis(20)),
                Some(Error::Unavailable) => admission.stop(),
                _ => {}
            }
            if let Some(expected) = failure {
                assert!(
                    matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
            }
            drop(wait);
            assert!(pool.waiting.borrow().is_empty());
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            drop(held);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
        }
    }

    #[test]
    fn pipes_are_nonblocking_bounded_cloexec_and_independent() {
        let admission = admission(2);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = PipePool::new(admission, reactor);
        let mut first = pool.acquire().unwrap();
        let mut second = pool.acquire().unwrap();
        for fd in [&first.read, &first.write] {
            // SAFETY: these descriptors are owned for the duration of the query.
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) } & libc::O_NONBLOCK,
                0
            );
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
                0
            );
        }
        assert!(first.capacity() <= MAX_PIPE_BYTES);
        let bytes = vec![0x5a; first.capacity()];
        assert_eq!(first.try_write(&bytes).unwrap(), bytes.len());
        assert_eq!(
            first.try_write(b"x").unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        let mut out = vec![0; bytes.len()];
        assert_eq!(
            second.try_read(&mut out).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(first.try_read(&mut out).unwrap(), bytes.len());
        assert_eq!(out, bytes);
        assert_eq!(
            first.try_read(&mut out).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(second.try_write(b"second").unwrap(), 6);
        assert_eq!(second.try_read(&mut out).unwrap(), 6);
        assert_eq!(&out[..6], b"second");
    }

    #[test]
    fn copied_splice_survives_source_reuse_and_partial_drain() {
        use std::{io::Read, os::unix::net::UnixStream};
        let admission = admission(1);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = PipePool::new(admission, reactor);
        let mut pipe = pool.acquire().unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        let mut source = b"copied kernel pages".to_vec();
        assert_eq!(pipe.try_write(&source).unwrap(), source.len());
        source.fill(0);
        assert_eq!(pipe.try_splice_to(&socket, 6).unwrap(), 6);
        assert_eq!(pipe.buffered(), 13);
        assert_eq!(pipe.try_splice_to(&socket, usize::MAX).unwrap(), 13);
        assert_eq!(pipe.buffered(), 0);
        // Socket-owned kernel pages survive both pipe closure and quota reuse.
        drop(pipe);
        let mut reused = pool.acquire().unwrap();
        reused.try_write(b"replacement data").unwrap();
        let mut received = [0; 19];
        peer.read_exact(&mut received).unwrap();
        assert_eq!(&received, b"copied kernel pages");
    }

    #[test]
    fn splice_rejects_blocking_socket_and_handles_disconnect_without_losing_bytes() {
        use std::os::unix::net::UnixStream;
        let admission = admission(1);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = PipePool::new(admission, reactor);
        let mut pipe = pool.acquire().unwrap();
        pipe.try_write(b"abc").unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        assert_eq!(
            pipe.try_splice_to(&socket, 3).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(pipe.buffered(), 3);
        socket.set_nonblocking(true).unwrap();
        drop(peer);
        assert_eq!(
            pipe.try_splice_to(&socket, 3).unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        assert_eq!(pipe.buffered(), 3);
        let mut bytes = [0; 3];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes, b"abc");
    }

    #[test]
    fn splice_backpressure_preserves_buffered_bytes_and_socket_validation() {
        use std::{io::Write, os::unix::net::UnixStream};
        let admission = admission(1);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = PipePool::new(admission, reactor);
        let mut pipe = pool.acquire().unwrap();
        pipe.try_write(b"pending").unwrap();
        let (mut socket, _peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        let start = std::time::Instant::now();
        loop {
            assert!(start.elapsed() < std::time::Duration::from_secs(5));
            match socket.write(&[1; 8192]) {
                Ok(count) => assert_ne!(count, 0),
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                Err(error) => panic!("{error}"),
            }
        }
        assert_eq!(
            pipe.try_splice_to(&socket, 7).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(pipe.buffered(), 7);
        let (datagram, _peer) = std::os::unix::net::UnixDatagram::pair().unwrap();
        datagram.set_nonblocking(true).unwrap();
        assert_eq!(
            pipe.try_splice_to(&datagram, 7).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        let file = std::fs::OpenOptions::new()
            .write(true)
            .open("/dev/null")
            .unwrap();
        // SAFETY: change flags on this exclusively owned test descriptor.
        assert_eq!(
            unsafe { libc::fcntl(file.as_raw_fd(), libc::F_SETFL, libc::O_NONBLOCK) },
            0
        );
        assert_eq!(
            pipe.try_splice_to(&file, 7).unwrap_err().raw_os_error(),
            Some(libc::ENOTSOCK)
        );
        let mut bytes = [0; 7];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 7);
        assert_eq!(&bytes, b"pending");
    }
}
