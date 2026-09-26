//! Concrete OS resource owner. Simulated resources never have a raw descriptor.
use crate::error::{Error, Result};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};

#[derive(Debug)]
pub enum Descriptor {
    Real(OwnedFd),
    #[cfg(test)]
    Sim(super::simulation::Handle),
}

impl Descriptor {
    /// Explicit extraction for host-only adapters. A simulated handle is rejected.
    pub fn into_host(self) -> std::result::Result<OwnedFd, Self> {
        match self {
            Self::Real(fd) => Ok(fd),
            #[cfg(test)]
            other => Err(other),
        }
    }
    pub fn tcp_listener(address: std::net::SocketAddr) -> Result<Self> {
        #[cfg(test)]
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
        #[cfg(test)]
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

    pub fn set_nonblocking(&self) -> Result<()> {
        #[cfg(test)]
        if matches!(self, Self::Sim(_)) {
            return Ok(());
        }
        let fd = self.as_raw_fd();
        unsafe {
            let flags = libc::fcntl(fd, libc::F_GETFL);
            if flags < 0
                || libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0
                || libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) < 0
            {
                return Err(Error::Io);
            }
        }
        Ok(())
    }

    pub fn try_send(&self, bytes: &[u8]) -> std::io::Result<usize> {
        #[cfg(test)]
        if let Self::Sim(handle) = self {
            return handle.send(bytes);
        }
        count(unsafe {
            libc::send(
                self.as_raw_fd(),
                bytes.as_ptr().cast(),
                bytes.len(),
                libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
            )
        })
    }
    pub fn try_recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
        #[cfg(test)]
        if let Self::Sim(handle) = self {
            return handle.recv(bytes);
        }
        count(unsafe {
            libc::recv(
                self.as_raw_fd(),
                bytes.as_mut_ptr().cast(),
                bytes.len(),
                libc::MSG_DONTWAIT,
            )
        })
    }

    pub fn idle_healthy(&self) -> bool {
        #[cfg(test)]
        if let Self::Sim(handle) = self {
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

    pub fn validate_socket(&self) -> Result<()> {
        #[cfg(test)]
        if let Self::Sim(handle) = self {
            return handle.validate_socket().map_err(|_| Error::InvalidRequest);
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
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }
}

pub(crate) fn count(value: isize) -> std::io::Result<usize> {
    if value < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

impl AsRawFd for Descriptor {
    fn as_raw_fd(&self) -> RawFd {
        match self {
            Self::Real(fd) => fd.as_raw_fd(),
            #[cfg(test)]
            Self::Sim(_) => panic!("simulated descriptor reached a host syscall"),
        }
    }
}
impl AsFd for Descriptor {
    fn as_fd(&self) -> BorrowedFd<'_> {
        match self {
            Self::Real(fd) => fd.as_fd(),
            #[cfg(test)]
            Self::Sim(_) => panic!("simulated descriptor reached a host syscall"),
        }
    }
}
impl FromRawFd for Descriptor {
    unsafe fn from_raw_fd(fd: RawFd) -> Self {
        Self::Real(unsafe { OwnedFd::from_raw_fd(fd) })
    }
}
macro_rules! from_host {
    ($($ty:ty),*) => { $(impl From<$ty> for Descriptor {
        fn from(value: $ty) -> Self { Self::Real(value.into()) }
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
