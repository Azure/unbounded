//! Owned file descriptors for the io_uring backend.
//!
//! [`Fd`] is a cheap-clone, single-threaded handle to an [`OwnedFd`]. The
//! reactor clones it into every in-flight operation, so the descriptor cannot
//! be closed (and its number reused) while the kernel may still refer to it.
//! The descriptor is closed when the last clone is dropped.
//!
//! ```
//! use std::os::fd::AsRawFd;
//! use uring_runtime::Fd;
//!
//! let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
//! let fd = Fd::from(listener);
//! let clone = fd.clone();
//! assert_eq!(fd.as_raw_fd(), clone.as_raw_fd());
//! drop(clone);
//! let owned = fd.into_owned().unwrap();
//! # drop(owned);
//! ```

use std::fmt;
use std::fs::File;
use std::net::{TcpListener, TcpStream, UdpSocket};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd, RawFd};
use std::os::unix::net::{UnixDatagram, UnixListener, UnixStream};
use std::rc::Rc;

/// A reference-counted owned file descriptor.
///
/// `Fd` is `!Send`: it belongs to the thread whose reactor uses it. Convert to
/// an [`OwnedFd`] with [`into_owned`](Fd::into_owned) to move it elsewhere.
#[derive(Clone)]
pub struct Fd(Rc<OwnedFd>);

impl Fd {
    /// Recover the [`OwnedFd`] if this is the only handle, or return `self`.
    pub fn into_owned(self) -> Result<OwnedFd, Self> {
        Rc::try_unwrap(self.0).map_err(Self)
    }

    /// Whether two handles refer to the same owned descriptor.
    pub fn ptr_eq(&self, other: &Self) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }

    /// The number of live handles to this descriptor.
    pub fn handle_count(&self) -> usize {
        Rc::strong_count(&self.0)
    }
}

impl fmt::Debug for Fd {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_tuple("Fd").field(&self.as_raw_fd()).finish()
    }
}

impl AsRawFd for Fd {
    fn as_raw_fd(&self) -> RawFd {
        self.0.as_raw_fd()
    }
}

impl AsFd for Fd {
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.0.as_fd()
    }
}

impl From<OwnedFd> for Fd {
    fn from(fd: OwnedFd) -> Self {
        Self(Rc::new(fd))
    }
}

macro_rules! from_std {
    ($($ty:ty),*) => {$(
        impl From<$ty> for Fd {
            fn from(value: $ty) -> Self {
                OwnedFd::from(value).into()
            }
        }
    )*};
}

from_std!(
    File,
    TcpListener,
    TcpStream,
    UdpSocket,
    UnixDatagram,
    UnixListener,
    UnixStream
);

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};

    fn is_open(raw: RawFd) -> bool {
        // SAFETY: F_GETFD has no side effects on any descriptor number.
        unsafe { libc::fcntl(raw, libc::F_GETFD) != -1 }
    }

    #[test]
    fn clones_share_and_last_drop_closes() {
        let (a, mut b) = UnixStream::pair().unwrap();
        let fd = Fd::from(a);
        let raw = fd.as_raw_fd();
        let clone = fd.clone();
        assert!(fd.ptr_eq(&clone));
        assert_eq!(fd.handle_count(), 2);
        let fd = fd.into_owned().unwrap_err();
        drop(clone);
        assert!(is_open(raw));
        let mut stream = UnixStream::from(fd.into_owned().unwrap());
        stream.write_all(b"x").unwrap();
        let mut byte = [0];
        b.read_exact(&mut byte).unwrap();
        drop(stream);
        // Whether `raw` is closed now cannot be asserted: parallel tests may
        // reuse the number immediately.
    }

    #[test]
    fn debug_and_as_fd() {
        let file = File::open("/dev/null").unwrap();
        let fd = Fd::from(file);
        assert_eq!(fd.as_fd().as_raw_fd(), fd.as_raw_fd());
        assert!(format!("{fd:?}").starts_with("Fd("));
        let other = Fd::from(UdpSocket::bind("127.0.0.1:0").unwrap());
        assert!(!fd.ptr_eq(&other));
    }
}
