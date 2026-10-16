//! Owned OS resources used with the Racer reactor.
//!
//! A [`Descriptor`] is the handle every reactor operation takes (usually as
//! `Rc<Descriptor>`). Today it always owns a host file descriptor; simulated
//! resources will be a second variant that never has a raw descriptor number,
//! so code that needs a host descriptor should use the fallible
//! [`Descriptor::as_host_fd`] or [`Descriptor::into_host`] rather than
//! [`AsRawFd`]/[`AsFd`], which panic for simulated handles.

use crate::{Error, Result};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};

/// An owned host (or, later, simulated) resource.
///
/// The host variant wraps a [`uring::Fd`] so the io_uring backend can borrow
/// it directly; this descriptor is its only handle.
#[derive(Debug)]
pub struct Descriptor(Kind);

/// Backend-specific ownership, never a fabricated raw descriptor.
#[derive(Debug)]
enum Kind {
    Host(uring::Fd),
    // TODO(sim): add `#[cfg(feature = "simulation")] Sim(uring_runtime_sim::SimFd)`
    // and route every host-only method below through the simulator, as the old
    // runtime did with `simulation::Handle`.
}

impl Descriptor {
    /// The library descriptor of a host resource, for the io_uring backend.
    pub(crate) fn host(&self) -> Option<&uring::Fd> {
        match &self.0 {
            Kind::Host(fd) => Some(fd),
        }
    }

    /// Borrow the host descriptor, or `None` for a simulated resource.
    pub fn as_host_fd(&self) -> Option<BorrowedFd<'_>> {
        self.host().map(AsFd::as_fd)
    }

    /// Whether this is a host descriptor (as opposed to a simulated one).
    pub fn is_host(&self) -> bool {
        self.host().is_some()
    }

    /// Set TCP_NODELAY for TCP callers, including disabling it when false.
    pub fn enable_tcp_nodelay(&self, enabled: bool) -> Result<()> {
        let value: libc::c_int = enabled.into();
        // SAFETY: the descriptor is owned and setsockopt reads one live integer.
        let result = unsafe {
            libc::setsockopt(
                self.as_raw_fd(),
                libc::IPPROTO_TCP,
                libc::TCP_NODELAY,
                (&value as *const libc::c_int).cast(),
                std::mem::size_of_val(&value) as libc::socklen_t,
            )
        };
        if result < 0 {
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Numeric socket identity only, sampled on failure while the owner is live.
    pub fn tcp_tuple(&self) -> Option<(std::net::SocketAddr, std::net::SocketAddr)> {
        let fd = self.as_host_fd()?;
        // SAFETY: ManuallyDrop borrows this live descriptor without closing it.
        let socket = std::mem::ManuallyDrop::new(unsafe {
            std::net::TcpStream::from_raw_fd(fd.as_raw_fd())
        });
        Some((socket.local_addr().ok()?, socket.peer_addr().ok()?))
    }

    /// The listener must already be nonblocking; accept4 flags apply only to the
    /// accepted socket. EINTR is retried a bounded number of times; other errno
    /// values, including EAGAIN, remain available to caller retry policy.
    pub fn try_accept(&self) -> std::io::Result<Self> {
        // SAFETY: live listener; successful descriptor is immediately owned.
        let fd = retry_interrupted(|| {
            count(unsafe {
                libc::accept4(
                    self.as_raw_fd(),
                    std::ptr::null_mut(),
                    std::ptr::null_mut(),
                    libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                ) as isize
            })
        })?;
        // SAFETY: accept4 returned a fresh descriptor that nothing else owns.
        Ok(unsafe { Self::from_raw_fd(fd as RawFd) })
    }

    /// Explicit extraction for host-only adapters. A simulated handle is
    /// rejected, and so is a host descriptor that is still referenced by the
    /// reactor (which cannot happen while the caller owns this value).
    pub fn into_host(self) -> std::result::Result<OwnedFd, Self> {
        match self.0 {
            Kind::Host(fd) => fd.into_owned().map_err(|fd| Self(Kind::Host(fd))),
        }
    }

    /// Bind a nonblocking TCP listener in the currently selected environment.
    pub fn tcp_listener(address: std::net::SocketAddr) -> Result<Self> {
        // TODO(sim): when the current environment is simulated, create the
        // listener in the simulated world instead.
        let listener = std::net::TcpListener::bind(address).map_err(|_| Error::Io)?;
        listener.set_nonblocking(true).map_err(|_| Error::Io)?;
        Ok(listener.into())
    }

    /// Create a nonblocking, close-on-exec stream socket for the requested family.
    pub fn socket(domain: i32) -> Result<Self> {
        // TODO(sim): when the current environment is simulated, create the
        // socket in the simulated world instead.
        // SAFETY: no pointers; the result is checked below.
        let raw = unsafe {
            libc::socket(
                domain,
                libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                0,
            )
        };
        if raw < 0 {
            return Err(Error::Io);
        }
        // SAFETY: socket returned a fresh descriptor that nothing else owns.
        Ok(unsafe { Self::from_raw_fd(raw) })
    }

    /// Enable O_NONBLOCK and FD_CLOEXEC, preserving other status/descriptor flags.
    /// O_NONBLOCK affects duplicated descriptors sharing the open file description;
    /// FD_CLOEXEC is local to this descriptor. Failure may leave partial changes.
    pub fn set_nonblocking(&self) -> Result<()> {
        let fd = self.as_raw_fd();
        // SAFETY: fcntl only inspects and updates flags of this live descriptor.
        unsafe {
            let flags = libc::fcntl(fd, libc::F_GETFL);
            let descriptor_flags = libc::fcntl(fd, libc::F_GETFD);
            if flags < 0
                || descriptor_flags < 0
                || libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0
                || libc::fcntl(fd, libc::F_SETFD, descriptor_flags | libc::FD_CLOEXEC) < 0
            {
                return Err(Error::Io);
            }
        }
        Ok(())
    }

    /// Send without waiting or raising SIGPIPE; preserve short progress and errno.
    /// Only zero-progress EINTR is retried, for at most four attempts.
    pub fn try_send(&self, bytes: &[u8]) -> std::io::Result<usize> {
        retry_interrupted(|| {
            // SAFETY: `bytes` is valid for reads of its length for this call.
            count(unsafe {
                libc::send(
                    self.as_raw_fd(),
                    bytes.as_ptr().cast(),
                    bytes.len(),
                    libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
                )
            })
        })
    }

    /// Receive without waiting; preserve EOF, short progress, and errno.
    /// Only zero-progress EINTR is retried, for at most four attempts.
    pub fn try_recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
        retry_interrupted(|| {
            // SAFETY: `bytes` is valid for writes of its length for this call.
            count(unsafe {
                libc::recv(
                    self.as_raw_fd(),
                    bytes.as_mut_ptr().cast(),
                    bytes.len(),
                    libc::MSG_DONTWAIT,
                )
            })
        })
    }

    /// Check for an idle connected socket without consuming any queued byte.
    /// Unexpected data, EOF, and errors all make a pooled socket unsuitable.
    pub fn idle_healthy(&self) -> bool {
        let mut byte = 0u8;
        // SAFETY: one writable local byte; MSG_PEEK consumes nothing.
        let result = unsafe {
            libc::recv(
                self.as_raw_fd(),
                (&mut byte as *mut u8).cast(),
                1,
                libc::MSG_PEEK | libc::MSG_DONTWAIT,
            )
        };
        result < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EAGAIN)
    }

    /// Full peer close, not a write-half shutdown. Consumes no bytes and never
    /// competes with parsing, even during acquisition or response writes.
    pub fn peer_disconnected(&self) -> bool {
        self.host_closed(0)
    }

    /// Unlike POLLHUP, POLLRDHUP notices a peer FIN while our send side is open.
    pub fn peer_read_closed(&self) -> bool {
        self.host_closed(libc::POLLRDHUP)
    }

    /// Shut down one or both socket directions without releasing ownership.
    pub fn shutdown(&self, how: i32) -> Result<()> {
        // SAFETY: shutdown only changes this live owned descriptor's state.
        if unsafe { libc::shutdown(self.as_raw_fd(), how) } < 0 {
            return Err(Error::from_io(std::io::Error::last_os_error()));
        }
        Ok(())
    }

    /// Require a stream socket, rejecting files and other socket kinds.
    pub fn validate_socket(&self) -> Result<()> {
        let mut kind: libc::c_int = 0;
        let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
        // SAFETY: getsockopt writes at most `length` bytes into the local.
        let result = unsafe {
            libc::getsockopt(
                self.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_TYPE,
                (&mut kind as *mut libc::c_int).cast(),
                &mut length,
            )
        };
        if result < 0 || kind != libc::SOCK_STREAM {
            return Err(Error::InvalidInput);
        }
        Ok(())
    }

    /// Inspect host closure flags without waiting or taking descriptor ownership.
    fn host_closed(&self, interest: i16) -> bool {
        let mut fd = libc::pollfd {
            fd: self.as_raw_fd(),
            events: interest,
            revents: 0,
        };
        // SAFETY: poll only inspects one live descriptor and never waits.
        (unsafe { libc::poll(&mut fd, 1, 0) > 0 })
            && fd.revents & (interest | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0
    }
}

impl AsRawFd for Descriptor {
    /// Borrow a host descriptor number.
    ///
    /// # Panics
    ///
    /// Panics for a simulated resource (not yet possible). Prefer
    /// [`Descriptor::as_host_fd`] in new code.
    fn as_raw_fd(&self) -> RawFd {
        match &self.0 {
            Kind::Host(fd) => fd.as_raw_fd(),
        }
    }
}

impl AsFd for Descriptor {
    /// Borrow host ownership for a syscall.
    ///
    /// # Panics
    ///
    /// Panics for a simulated resource (not yet possible). Prefer
    /// [`Descriptor::as_host_fd`] in new code.
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.as_host_fd()
            .expect("simulated descriptor reached a host syscall")
    }
}

impl FromRawFd for Descriptor {
    /// Assume sole closing ownership of a live host descriptor.
    unsafe fn from_raw_fd(fd: RawFd) -> Self {
        // SAFETY: forwarded from the caller.
        Self::from(unsafe { OwnedFd::from_raw_fd(fd) })
    }
}

impl From<uring::Fd> for Descriptor {
    /// Wrap a library descriptor (for example one adopted by the backend).
    fn from(fd: uring::Fd) -> Self {
        Self(Kind::Host(fd))
    }
}

/// Implement closing-ownership transfer for supported host resource adapters.
macro_rules! from_host {
    ($($ty:ty),*) => { $(impl From<$ty> for Descriptor {
        /// Transfer a host resource's closing ownership into the reactor adapter.
        fn from(value: $ty) -> Self { Self(Kind::Host(uring::Fd::from(OwnedFd::from(value)))) }
    })* };
}

from_host!(
    OwnedFd,
    std::fs::File,
    std::net::TcpStream,
    std::net::TcpListener,
    std::net::UdpSocket,
    std::os::unix::net::UnixStream,
    std::os::unix::net::UnixListener,
    std::os::unix::net::UnixDatagram
);

/// Convert a syscall byte count without losing errno on failure.
fn count(value: isize) -> std::io::Result<usize> {
    if value < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

/// Bound zero-progress EINTR retries, returning short success and other errors.
fn retry_interrupted<T>(mut syscall: impl FnMut() -> std::io::Result<T>) -> std::io::Result<T> {
    for _ in 0..3 {
        match syscall() {
            Err(error) if error.raw_os_error() == Some(libc::EINTR) => (),
            result => return result,
        }
    }
    syscall()
}

#[cfg(test)]
mod tests {
    //! Host descriptor flags, bounded retries, and ownership extraction.
    use super::*;

    /// Preserve short success and errno while bounding interrupted syscall retries.
    #[test]
    fn interrupted_syscalls_retry_boundedly_without_hiding_errno_or_short_success() {
        let mut calls = 0;
        let result: std::io::Result<()> = retry_interrupted(|| {
            calls += 1;
            Err(std::io::Error::from_raw_os_error(libc::EINTR))
        });
        assert_eq!(calls, 4);
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EINTR));
        let mut calls = 0;
        assert_eq!(
            retry_interrupted(|| {
                calls += 1;
                if calls == 1 {
                    Err(std::io::Error::from_raw_os_error(libc::EINTR))
                } else {
                    Ok(1)
                }
            })
            .unwrap(),
            1
        );
        assert_eq!(calls, 2);
        let result: std::io::Result<()> =
            retry_interrupted(|| Err(std::io::Error::from_raw_os_error(libc::EAGAIN)));
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EAGAIN));
    }

    /// Check reversible TCP_NODELAY and nonblocking close-on-exec configuration.
    #[test]
    fn nodelay_can_be_disabled_and_nonblocking_sets_cloexec() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let client = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let fd = Descriptor::from(client.try_clone().unwrap());
        fd.enable_tcp_nodelay(true).unwrap();
        assert!(client.nodelay().unwrap());
        fd.enable_tcp_nodelay(false).unwrap();
        assert!(!client.nodelay().unwrap());
        fd.set_nonblocking().unwrap();
        // SAFETY: fcntl only reads flags of a live descriptor.
        assert_ne!(
            unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) } & libc::O_NONBLOCK,
            0
        );
        // SAFETY: as above.
        assert_ne!(
            unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
        let (local, peer) = fd.tcp_tuple().unwrap();
        assert_eq!(peer, listener.local_addr().unwrap());
        assert_eq!(local, client.local_addr().unwrap());
        fd.validate_socket().unwrap();
    }

    /// Keep the response direction usable after a peer's write-half close.
    #[test]
    fn peer_write_half_close_does_not_imply_full_disconnect() {
        let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
        let fd = Descriptor::from(left);
        right.shutdown(std::net::Shutdown::Write).unwrap();
        assert!(fd.peer_read_closed());
        assert!(!fd.peer_disconnected());
        assert_eq!(fd.try_send(b"response").unwrap(), 8);
        drop(right);
        assert!(fd.peer_disconnected());
    }

    /// Half-close, EOF, and invalid shutdown modes behave like the old host path.
    #[test]
    fn descriptor_half_close_and_shutdown_errors() {
        let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
        let (left, right) = (Descriptor::from(left), Descriptor::from(right));
        assert!(right.idle_healthy());
        left.try_send(b"x").unwrap();
        assert!(!right.idle_healthy());
        left.shutdown(libc::SHUT_WR).unwrap();
        assert!(right.peer_read_closed());
        assert!(!right.peer_disconnected());
        assert_eq!(right.try_recv(&mut [0; 1]).unwrap(), 1);
        assert_eq!(right.try_recv(&mut [0; 1]).unwrap(), 0);
        assert_eq!(right.try_send(b"r").unwrap(), 1);
        assert_eq!(left.try_recv(&mut [0; 1]).unwrap(), 1);
        assert_eq!(left.shutdown(123), Err(Error::Os(libc::EINVAL)));
        drop(left);
        assert!(right.peer_disconnected());
    }

    /// Files are not stream sockets; listeners accept nonblocking peers.
    #[test]
    fn validation_listener_accept_and_host_extraction() {
        let file = Descriptor::from(std::fs::File::open("/proc/self/stat").unwrap());
        assert_eq!(file.validate_socket(), Err(Error::InvalidInput));
        assert!(file.is_host());
        assert!(file.as_host_fd().is_some());
        let listener = Descriptor::tcp_listener("127.0.0.1:0".parse().unwrap()).unwrap();
        assert_eq!(
            listener.try_accept().unwrap_err().raw_os_error(),
            Some(libc::EAGAIN)
        );
        let address = {
            let owned = listener.into_host().unwrap();
            let std = std::net::TcpListener::from(owned);
            let address = std.local_addr().unwrap();
            let listener = Descriptor::from(std);
            let _client = std::net::TcpStream::connect(address).unwrap();
            let accepted = loop {
                match listener.try_accept() {
                    Ok(fd) => break fd,
                    Err(error) if error.raw_os_error() == Some(libc::EAGAIN) => {
                        std::thread::yield_now()
                    }
                    Err(error) => panic!("{error}"),
                }
            };
            // SAFETY: fcntl only reads flags of a live descriptor.
            let flags = unsafe { libc::fcntl(accepted.as_raw_fd(), libc::F_GETFL) };
            assert_ne!(flags & libc::O_NONBLOCK, 0);
            address
        };
        assert!(address.port() != 0);
        let socket = Descriptor::socket(libc::AF_INET).unwrap();
        socket.validate_socket().unwrap();
    }
}
