//! Concrete OS resource owner. Simulated resources never have a raw descriptor.
use crate::{Error, Result};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};

/// Owned host or simulation resource. `AsRawFd` and `AsFd` panic for simulated
/// handles: use `as_sim` or fallible `into_host` at explicit backend boundaries.
#[derive(Debug)]
pub struct Descriptor(Kind);
#[derive(Debug)]
pub(super) enum Kind {
    Real(OwnedFd),
    #[cfg(feature = "simulation")]
    Sim(super::simulation::Handle),
}

impl Descriptor {
    #[cfg(feature = "simulation")]
    pub fn as_sim(&self) -> Option<&super::simulation::Handle> {
        match &self.0 {
            Kind::Sim(handle) => Some(handle),
            Kind::Real(_) => None,
        }
    }
    #[cfg(feature = "simulation")]
    pub fn into_sim(self) -> std::result::Result<super::simulation::Handle, Self> {
        match self.0 {
            Kind::Sim(handle) => Ok(handle),
            other => Err(Self(other)),
        }
    }
    /// Set TCP_NODELAY for TCP callers, including disabling it when false.
    pub fn enable_tcp_nodelay(&self, enabled: bool) -> Result<()> {
        #[cfg(feature = "simulation")]
        if self.as_sim().is_some() {
            return Ok(());
        }
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
        #[cfg(feature = "simulation")]
        if self.as_sim().is_some() {
            return None;
        }
        // SAFETY: ManuallyDrop borrows this live descriptor without closing it.
        let socket = std::mem::ManuallyDrop::new(unsafe {
            std::net::TcpStream::from_raw_fd(self.as_raw_fd())
        });
        Some((socket.local_addr().ok()?, socket.peer_addr().ok()?))
    }
    /// The listener must already be nonblocking; accept4 flags apply only to the
    /// accepted socket. EINTR is retried a bounded number of times; other errno
    /// values, including EAGAIN, remain available to caller retry policy.
    pub fn try_accept(&self) -> std::io::Result<Self> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.accept();
        }
        // SAFETY: the listener stays owned during this nonblocking syscall; the
        // returned descriptor is immediately wrapped in its unique owner.
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
        Ok(unsafe { Self::from_raw_fd(fd as RawFd) })
    }
    /// Explicit extraction for host-only adapters. A simulated handle is rejected.
    pub fn into_host(self) -> std::result::Result<OwnedFd, Self> {
        match self.0 {
            Kind::Real(fd) => Ok(fd),
            #[cfg(feature = "simulation")]
            other => Err(Self(other)),
        }
    }
    pub fn tcp_listener(address: std::net::SocketAddr) -> Result<Self> {
        #[cfg(feature = "simulation")]
        if let Some(sim) = super::simulation::Simulation::current() {
            return sim
                .listen(super::SocketAddress::Inet(address))
                .map_err(|_| Error::Io);
        }
        let listener = std::net::TcpListener::bind(address).map_err(|_| Error::Io)?;
        listener.set_nonblocking(true).map_err(|_| Error::Io)?;
        Ok(listener.into())
    }
    pub fn socket(domain: i32) -> Result<Self> {
        #[cfg(feature = "simulation")]
        if let Some(sim) = super::simulation::Simulation::current() {
            return sim.socket(domain).map_err(|_| Error::Io);
        }
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
        Ok(unsafe { Self::from_raw_fd(raw) })
    }

    /// Enable O_NONBLOCK and FD_CLOEXEC, preserving other status/descriptor flags.
    /// O_NONBLOCK affects duplicated descriptors sharing the open file description;
    /// FD_CLOEXEC is local to this descriptor. Failure may leave partial changes.
    pub fn set_nonblocking(&self) -> Result<()> {
        #[cfg(feature = "simulation")]
        if self.as_sim().is_some() {
            return Ok(());
        }
        let fd = self.as_raw_fd();
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

    pub fn try_send(&self, bytes: &[u8]) -> std::io::Result<usize> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.send(bytes);
        }
        retry_interrupted(|| {
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
    pub fn try_recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.recv(bytes);
        }
        retry_interrupted(|| {
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

    pub fn idle_healthy(&self) -> bool {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.idle_healthy();
        }
        let mut byte = 0u8;
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

    /// Full peer close, not a write-half shutdown. Safe during response writes
    /// and acquisition: consumes no bytes and never competes with HTTP parsing.
    pub fn peer_disconnected(&self) -> bool {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.peer_disconnected();
        }
        let mut fd = libc::pollfd {
            fd: self.as_raw_fd(),
            events: 0,
            revents: 0,
        };
        (unsafe { libc::poll(&mut fd, 1, 0) > 0 })
            && fd.revents & (libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0
    }
    /// Unlike POLLHUP, POLLRDHUP also notices a peer FIN while our send side is open.
    pub fn peer_read_closed(&self) -> bool {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.peer_read_closed();
        }
        let mut fd = libc::pollfd {
            fd: self.as_raw_fd(),
            events: libc::POLLRDHUP,
            revents: 0,
        };
        // SAFETY: poll only inspects one live descriptor and never waits.
        (unsafe { libc::poll(&mut fd, 1, 0) > 0 })
            && fd.revents & (libc::POLLRDHUP | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0
    }

    /// Shut down one or both socket directions without releasing ownership.
    pub fn shutdown(&self, how: i32) -> Result<()> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.shutdown(how).map_err(Error::from_io);
        }
        // SAFETY: shutdown only changes the state of this live owned descriptor.
        if unsafe { libc::shutdown(self.as_raw_fd(), how) } < 0 {
            return Err(Error::from_io(std::io::Error::last_os_error()));
        }
        Ok(())
    }

    pub fn validate_socket(&self) -> Result<()> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = self.as_sim() {
            return handle.validate_socket().map_err(|_| Error::InvalidInput);
        }
        let mut kind: libc::c_int = 0;
        let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
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
}

#[cfg(all(test, feature = "simulation"))]
#[test]
fn descriptor_half_close_matches_simulated_and_host_sockets() {
    let sim = super::simulation::Simulation::new();
    let simulated = sim.socket_pair();
    let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
    for (left, right) in [simulated, (left.into(), right.into())] {
        left.try_send(b"x").unwrap();
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
}

pub(crate) fn count(value: isize) -> std::io::Result<usize> {
    if value < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

fn retry_interrupted<T>(mut syscall: impl FnMut() -> std::io::Result<T>) -> std::io::Result<T> {
    // EINTR reports no transferred bytes for these syscalls. Never retry a short
    // success or hide EAGAIN/connection failures. Bound signal-storm work per turn.
    for _ in 0..3 {
        match syscall() {
            Err(error) if error.raw_os_error() == Some(libc::EINTR) => (),
            result => return result,
        }
    }
    syscall()
}

impl AsRawFd for Descriptor {
    fn as_raw_fd(&self) -> RawFd {
        match &self.0 {
            Kind::Real(fd) => fd.as_raw_fd(),
            #[cfg(feature = "simulation")]
            Kind::Sim(_) => panic!("simulated descriptor reached a host syscall"),
        }
    }
}
impl AsFd for Descriptor {
    fn as_fd(&self) -> BorrowedFd<'_> {
        match &self.0 {
            Kind::Real(fd) => fd.as_fd(),
            #[cfg(feature = "simulation")]
            Kind::Sim(_) => panic!("simulated descriptor reached a host syscall"),
        }
    }
}
impl FromRawFd for Descriptor {
    unsafe fn from_raw_fd(fd: RawFd) -> Self {
        Self(Kind::Real(unsafe { OwnedFd::from_raw_fd(fd) }))
    }
}
macro_rules! from_host {
    ($($ty:ty),*) => { $(impl From<$ty> for Descriptor {
        fn from(value: $ty) -> Self { Self(Kind::Real(value.into())) }
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

#[cfg(feature = "simulation")]
impl From<super::simulation::Handle> for Descriptor {
    fn from(handle: super::simulation::Handle) -> Self {
        Self(Kind::Sim(handle))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
        assert_ne!(
            unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) } & libc::O_NONBLOCK,
            0
        );
        assert_ne!(
            unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
    }

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

    #[cfg(feature = "simulation")]
    #[test]
    fn raw_simulated_descriptor_access_panics_and_host_extraction_is_fallible() {
        let sim = super::super::simulation::Simulation::new();
        let (fd, _peer) = sim.socket_pair();
        assert!(std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| fd.as_raw_fd())).is_err());
        assert!(std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| fd.as_fd())).is_err());
        assert!(fd.into_host().is_err());
    }
}
