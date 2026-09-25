//! Bounded, nonblocking, per-reader kernel pipes.
//!
//! Each lease owns both descriptors and its admission charge. No descriptor or
//! backing page is recycled while a reader owns the lease. Writes copy into kernel
//! pipe pages before splice: socket acceptance is not a userspace page reuse fence.
use crate::{
    error::{Error, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        reactor::Reactor,
    },
};
use std::{
    io,
    os::fd::{AsFd, AsRawFd, FromRawFd, OwnedFd},
    rc::Rc,
};

/// Maximum kernel buffer capacity per admitted reader. Pipe admission is in pipe
/// units, so total pipe capacity is bounded by Limits::pipes * MAX_PIPE_BYTES.
/// Spliced bytes retained by sockets are subject to socket buffer limits instead.
pub const MAX_PIPE_BYTES: usize = 64 * 1024;

pub struct PipePool {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
}

pub struct PipeLease {
    read: OwnedFd,
    write: OwnedFd,
    capacity: usize,
    buffered: usize,
    // Declared after the descriptors so capacity is returned only after closing.
    _reservation: Reservation,
}

impl PipePool {
    pub fn new(admission: Rc<Admission>, reactor: Rc<Reactor>) -> Self {
        Self { admission, reactor }
    }

    pub(crate) fn admission(&self) -> &Admission {
        &self.admission
    }

    pub(crate) fn reactor(&self) -> &Reactor {
        &self.reactor
    }

    /// Reserve before creating descriptors. Exhaustion never waits for a reader.
    pub fn acquire(&self) -> Result<PipeLease> {
        let reservation = self.admission.reserve(None, ResourceClass::Pipe, 1)?;
        reservation.validate(ResourceClass::Pipe, 1)?;
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
            route_search_work: small,
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
