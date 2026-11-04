//! Owned descriptors for either backend, and owned socket addresses.
use crate::{Error, Result, sim::SimOs};
use std::{
    net::SocketAddr,
    os::{
        fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, IntoRawFd, OwnedFd, RawFd},
        unix::ffi::OsStrExt,
    },
    path::PathBuf,
};

/// An owned descriptor of the host kernel or of a simulated OS.
///
/// Simulated descriptors are numbers in their world's private namespace; they
/// never reach a host syscall. [`Descriptor::host`] is `None` for them.
#[derive(Debug)]
pub struct Descriptor {
    raw: RawFd,

    backend: Backend,
}

#[derive(Debug)]
enum Backend {
    Host,
    /// A host descriptor owned elsewhere; never closed here.
    Borrowed,
    Sim(SimOs),
}

impl Descriptor {
    /// Wrap a simulated descriptor number owned by `os`.
    pub(crate) fn sim(os: SimOs, raw: RawFd) -> Self {
        Self {
            raw,
            backend: Backend::Sim(os),
        }
    }

    /// Refer to a host descriptor owned elsewhere; dropping never closes it.
    ///
    /// # Safety
    /// `raw` must stay open for as long as this value and every operation
    /// using it are alive.
    pub unsafe fn borrowed(raw: RawFd) -> Self {
        Self {
            raw,
            backend: Backend::Borrowed,
        }
    }

    /// The descriptor number in its backend's namespace.
    pub fn raw(&self) -> RawFd {
        self.raw
    }

    /// Borrow the host descriptor, or `None` for a simulated one.
    pub fn host(&self) -> Option<BorrowedFd<'_>> {
        match self.backend {
            // SAFETY: a host descriptor stays open for the lifetime of self.
            Backend::Host | Backend::Borrowed => Some(unsafe { BorrowedFd::borrow_raw(self.raw) }),
            Backend::Sim(_) => None,
        }
    }

    /// The simulated OS owning this descriptor, if any.
    pub fn sim_os(&self) -> Option<&SimOs> {
        match &self.backend {
            Backend::Sim(os) => Some(os),
            _ => None,
        }
    }

    /// Whether this is a simulated descriptor.
    pub fn is_sim(&self) -> bool {
        matches!(self.backend, Backend::Sim(_))
    }

    /// Extract the host owner, returning `self` unchanged for a simulated one.
    pub fn into_host(self) -> std::result::Result<OwnedFd, Self> {
        if !matches!(self.backend, Backend::Host) {
            return Err(self);
        }
        let raw = self.raw;
        std::mem::forget(self);
        // SAFETY: ownership of the host descriptor moves to the OwnedFd.
        Ok(unsafe { OwnedFd::from_raw_fd(raw) })
    }

    /// Release ownership of the number without closing it.
    pub(crate) fn into_raw(self) -> RawFd {
        let raw = self.raw;
        // Drop the SimOs handle without running close.
        let this = std::mem::ManuallyDrop::new(self);
        // SAFETY: the backend is read exactly once and never used again.
        drop(unsafe { std::ptr::read(&this.backend) });
        raw
    }

    /// Create a nonblocking, close-on-exec host stream socket.
    pub fn socket(domain: i32) -> Result<Self> {
        // SAFETY: socket(2) has no memory preconditions.
        let raw = unsafe {
            libc::socket(
                domain,
                libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                0,
            )
        };
        if raw < 0 {
            return Err(Error::last_os_error());
        }
        // SAFETY: socket returned a fresh descriptor.
        Ok(unsafe { Self::from_raw_fd(raw) })
    }

    /// Bind a nonblocking host TCP listener.
    pub fn tcp_listener(address: SocketAddr) -> Result<Self> {
        let listener = std::net::TcpListener::bind(address)?;
        listener.set_nonblocking(true)?;
        Ok(listener.into())
    }

    /// Set or clear `TCP_NODELAY`; a no-op for simulated sockets.
    pub fn enable_tcp_nodelay(&self, enabled: bool) -> Result<()> {
        if self.is_sim() {
            return Ok(());
        }
        let value: libc::c_int = enabled.into();
        // SAFETY: setsockopt reads one live integer.
        let result = unsafe {
            libc::setsockopt(
                self.raw,
                libc::IPPROTO_TCP,
                libc::TCP_NODELAY,
                (&value as *const libc::c_int).cast(),
                std::mem::size_of_val(&value) as libc::socklen_t,
            )
        };
        if result < 0 {
            return Err(Error::last_os_error());
        }
        Ok(())
    }

    /// Local and peer TCP addresses of a host socket.
    pub fn tcp_tuple(&self) -> Option<(SocketAddr, SocketAddr)> {
        let fd = self.host()?;
        // SAFETY: ManuallyDrop borrows the live descriptor without closing it.
        let socket = std::mem::ManuallyDrop::new(unsafe {
            std::net::TcpStream::from_raw_fd(fd.as_raw_fd())
        });
        Some((socket.local_addr().ok()?, socket.peer_addr().ok()?))
    }

    /// Enable `O_NONBLOCK` and `FD_CLOEXEC`; a no-op for simulated descriptors.
    pub fn set_nonblocking(&self) -> Result<()> {
        if self.is_sim() {
            return Ok(());
        }
        let fd = self.raw;
        // SAFETY: fcntl only changes flags of this owned descriptor.
        unsafe {
            let flags = libc::fcntl(fd, libc::F_GETFL);
            let descriptor_flags = libc::fcntl(fd, libc::F_GETFD);
            if flags < 0
                || descriptor_flags < 0
                || libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0
                || libc::fcntl(fd, libc::F_SETFD, descriptor_flags | libc::FD_CLOEXEC) < 0
            {
                return Err(Error::last_os_error());
            }
        }
        Ok(())
    }

    /// Accept one connection without waiting (`accept4`, nonblocking, cloexec).
    pub fn try_accept(&self) -> std::io::Result<Self> {
        match &self.backend {
            Backend::Sim(os) => os.try_accept(self.raw),
            _ => {
                let fd = retry_interrupted(|| {
                    // SAFETY: live listener; a result is immediately owned.
                    count(unsafe {
                        libc::accept4(
                            self.raw,
                            std::ptr::null_mut(),
                            std::ptr::null_mut(),
                            libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                        ) as isize
                    })
                })?;
                // SAFETY: accept4 returned a fresh descriptor.
                Ok(unsafe { Self::from_raw_fd(fd as RawFd) })
            }
        }
    }

    /// Send without waiting or raising `SIGPIPE`.
    pub fn try_send(&self, bytes: &[u8]) -> std::io::Result<usize> {
        match &self.backend {
            Backend::Sim(os) => os.try_send(self.raw, bytes),
            _ => retry_interrupted(|| {
                // SAFETY: send reads the borrowed slice synchronously.
                count(unsafe {
                    libc::send(
                        self.raw,
                        bytes.as_ptr().cast(),
                        bytes.len(),
                        libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
                    )
                })
            }),
        }
    }

    /// Receive without waiting; `Ok(0)` is EOF.
    pub fn try_recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
        match &self.backend {
            Backend::Sim(os) => os.try_recv(self.raw, bytes),
            _ => retry_interrupted(|| {
                // SAFETY: recv writes the borrowed slice synchronously.
                count(unsafe {
                    libc::recv(
                        self.raw,
                        bytes.as_mut_ptr().cast(),
                        bytes.len(),
                        libc::MSG_DONTWAIT,
                    )
                })
            }),
        }
    }

    /// Whether an idle pooled socket has no queued byte, EOF, or error.
    pub fn idle_healthy(&self) -> bool {
        match &self.backend {
            Backend::Sim(os) => os.idle_healthy(self.raw),
            _ => {
                let mut byte = 0u8;
                // SAFETY: peeks at most one byte into a local.
                let result = unsafe {
                    libc::recv(
                        self.raw,
                        (&mut byte as *mut u8).cast(),
                        1,
                        libc::MSG_PEEK | libc::MSG_DONTWAIT,
                    )
                };
                result < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EAGAIN)
            }
        }
    }

    /// Whether the peer fully closed (not just a write-half shutdown).
    pub fn peer_disconnected(&self) -> bool {
        match &self.backend {
            Backend::Sim(os) => os.peer_disconnected(self.raw),
            _ => host_closed(self.raw, 0),
        }
    }

    /// Whether the peer shut down its write half (`POLLRDHUP`).
    pub fn peer_read_closed(&self) -> bool {
        match &self.backend {
            Backend::Sim(os) => os.peer_read_closed(self.raw),
            _ => host_closed(self.raw, libc::POLLRDHUP),
        }
    }

    /// Shut down one or both directions without releasing ownership.
    pub fn shutdown(&self, how: i32) -> Result<()> {
        match &self.backend {
            Backend::Sim(os) => os.shutdown(self.raw, how).map_err(Error::from_io),
            _ => {
                // SAFETY: shutdown only changes this live descriptor's state.
                if unsafe { libc::shutdown(self.raw, how) } < 0 {
                    return Err(Error::last_os_error());
                }
                Ok(())
            }
        }
    }

    /// Require a stream socket.
    pub fn validate_socket(&self) -> Result<()> {
        match &self.backend {
            Backend::Sim(os) => os
                .validate_socket(self.raw)
                .map_err(|_| Error::InvalidInput),
            _ => {
                let mut kind: libc::c_int = 0;
                let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
                // SAFETY: getsockopt writes one integer into a local.
                let result = unsafe {
                    libc::getsockopt(
                        self.raw,
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
    }
}

impl Drop for Descriptor {
    /// Close the descriptor in its backend.
    fn drop(&mut self) {
        match &self.backend {
            Backend::Host => {
                // SAFETY: this descriptor is uniquely owned.
                unsafe { libc::close(self.raw) };
            }
            Backend::Borrowed => (),
            Backend::Sim(os) => os.close(self.raw),
        }
    }
}

impl FromRawFd for Descriptor {
    /// Assume closing ownership of a live host descriptor.
    unsafe fn from_raw_fd(raw: RawFd) -> Self {
        Self {
            raw,
            backend: Backend::Host,
        }
    }
}

impl From<OwnedFd> for Descriptor {
    /// Take closing ownership of a host descriptor.
    fn from(fd: OwnedFd) -> Self {
        // SAFETY: ownership moves out of the OwnedFd.
        unsafe { Self::from_raw_fd(fd.into_raw_fd()) }
    }
}

macro_rules! from_host {
    ($($ty:ty),*) => { $(impl From<$ty> for Descriptor {
        /// Take closing ownership of a host resource.
        fn from(value: $ty) -> Self { OwnedFd::from(value).into() }
    })* };
}

from_host!(
    std::fs::File,
    std::net::TcpStream,
    std::net::TcpListener,
    std::net::UdpSocket,
    std::os::unix::net::UnixStream,
    std::os::unix::net::UnixListener,
    std::os::unix::net::UnixDatagram
);

impl AsRawFd for Descriptor {
    /// The host descriptor number.
    ///
    /// # Panics
    /// Panics for a simulated descriptor; use [`Descriptor::raw`] instead.
    fn as_raw_fd(&self) -> RawFd {
        assert!(
            !self.is_sim(),
            "simulated descriptor reached a host syscall"
        );
        self.raw
    }
}

impl AsFd for Descriptor {
    /// Borrow a host descriptor.
    ///
    /// # Panics
    /// Panics for a simulated descriptor; use [`Descriptor::host`] instead.
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.host()
            .expect("simulated descriptor reached a host syscall")
    }
}

/// Convert a syscall byte count without losing errno.
fn count(value: isize) -> std::io::Result<usize> {
    if value < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

/// Retry zero-progress `EINTR` at most three times.
fn retry_interrupted<T>(mut syscall: impl FnMut() -> std::io::Result<T>) -> std::io::Result<T> {
    for _ in 0..3 {
        match syscall() {
            Err(error) if error.raw_os_error() == Some(libc::EINTR) => (),
            result => return result,
        }
    }
    syscall()
}

/// Inspect host closure flags without waiting.
fn host_closed(fd: RawFd, interest: i16) -> bool {
    let mut poll = libc::pollfd {
        fd,
        events: interest,
        revents: 0,
    };
    // SAFETY: poll inspects one live descriptor and never waits.
    (unsafe { libc::poll(&mut poll, 1, 0) > 0 })
        && poll.revents & (interest | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0
}

/// An owned socket address.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum SocketAddress {
    /// IPv4 or IPv6 address including port and IPv6 scope.
    Inet(SocketAddr),
    /// Filesystem Unix socket path (abstract addresses are unsupported).
    Unix(PathBuf),
}

impl From<SocketAddr> for SocketAddress {
    /// An Internet address.
    fn from(address: SocketAddr) -> Self {
        Self::Inet(address)
    }
}

/// Encode an address into caller-provided `sockaddr_storage`.
pub(crate) fn encode_address(
    address: &SocketAddress,
    storage: &mut libc::sockaddr_storage,
) -> Result<libc::socklen_t> {
    // SAFETY: all-zero sockaddr storage is valid for every family.
    *storage = unsafe { std::mem::zeroed() };
    let base = (storage as *mut libc::sockaddr_storage).cast::<u8>();
    let len = match address {
        SocketAddress::Inet(SocketAddr::V4(address)) => {
            let value = libc::sockaddr_in {
                sin_family: libc::AF_INET as _,
                sin_port: address.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from_ne_bytes(address.ip().octets()),
                },
                sin_zero: [0; 8],
            };
            // SAFETY: sockaddr_storage is large and aligned enough.
            unsafe { base.cast::<libc::sockaddr_in>().write(value) };
            std::mem::size_of::<libc::sockaddr_in>()
        }
        SocketAddress::Inet(SocketAddr::V6(address)) => {
            let value = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as _,
                sin6_port: address.port().to_be(),
                sin6_flowinfo: address.flowinfo().to_be(),
                sin6_addr: libc::in6_addr {
                    s6_addr: address.ip().octets(),
                },
                sin6_scope_id: address.scope_id(),
            };
            // SAFETY: as above.
            unsafe { base.cast::<libc::sockaddr_in6>().write(value) };
            std::mem::size_of::<libc::sockaddr_in6>()
        }
        SocketAddress::Unix(path) => {
            let bytes = path.as_os_str().as_bytes();
            // SAFETY: all-zero sockaddr_un is valid.
            let mut value: libc::sockaddr_un = unsafe { std::mem::zeroed() };
            if bytes.is_empty() || bytes.contains(&0) || bytes.len() >= value.sun_path.len() {
                return Err(Error::InvalidInput);
            }
            value.sun_family = libc::AF_UNIX as _;
            for (dst, src) in value.sun_path.iter_mut().zip(bytes) {
                *dst = *src as libc::c_char;
            }
            // SAFETY: as above.
            unsafe { base.cast::<libc::sockaddr_un>().write(value) };
            std::mem::offset_of!(libc::sockaddr_un, sun_path) + bytes.len() + 1
        }
    };
    Ok(len as libc::socklen_t)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn interrupted_syscalls_retry_boundedly() {
        let mut calls = 0;
        let result: std::io::Result<()> = retry_interrupted(|| {
            calls += 1;
            Err(std::io::Error::from_raw_os_error(libc::EINTR))
        });
        assert_eq!(calls, 4);
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EINTR));
        let result: std::io::Result<()> =
            retry_interrupted(|| Err(std::io::Error::from_raw_os_error(libc::EAGAIN)));
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EAGAIN));
    }

    #[test]
    fn nodelay_nonblocking_and_half_close() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let client = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let fd = Descriptor::from(client.try_clone().unwrap());
        fd.enable_tcp_nodelay(true).unwrap();
        assert!(client.nodelay().unwrap());
        fd.enable_tcp_nodelay(false).unwrap();
        assert!(!client.nodelay().unwrap());
        fd.set_nonblocking().unwrap();
        assert!(fd.tcp_tuple().is_some());
        fd.validate_socket().unwrap();

        let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
        let fd = Descriptor::from(left);
        right.shutdown(std::net::Shutdown::Write).unwrap();
        assert!(fd.peer_read_closed());
        assert!(!fd.peer_disconnected());
        assert_eq!(fd.try_send(b"response").unwrap(), 8);
        drop(right);
        assert!(fd.peer_disconnected());
        let owned = fd.into_host().unwrap();
        assert!(owned.as_raw_fd() >= 0);
    }

    #[test]
    fn addresses_encode_and_reject_bad_unix_paths() {
        // SAFETY: zeroed storage is valid.
        let mut storage = unsafe { std::mem::zeroed() };
        let v4 = SocketAddress::Inet("127.0.0.1:80".parse().unwrap());
        assert_eq!(
            encode_address(&v4, &mut storage).unwrap() as usize,
            std::mem::size_of::<libc::sockaddr_in>()
        );
        let v6 = SocketAddress::Inet("[::1]:80".parse().unwrap());
        assert_eq!(
            encode_address(&v6, &mut storage).unwrap() as usize,
            std::mem::size_of::<libc::sockaddr_in6>()
        );
        for bad in ["", "a\0b"] {
            assert_eq!(
                encode_address(&SocketAddress::Unix(bad.into()), &mut storage),
                Err(Error::InvalidInput)
            );
        }
        assert!(encode_address(&SocketAddress::Unix("/tmp/x".into()), &mut storage).is_ok());
    }
}
