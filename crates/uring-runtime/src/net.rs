//! Socket addresses and their kernel encoding.
//!
//! [`SocketAddress`] is the address type used throughout the runtime.
//! [`SockAddr`] is its `sockaddr` encoding in heap storage whose address is
//! stable across moves, so it can be referenced by an in-flight `connect`.
//!
//! ```
//! use uring_runtime::{SockAddr, SocketAddress};
//!
//! let address = SocketAddress::from("127.0.0.1:8080".parse::<std::net::SocketAddr>().unwrap());
//! let encoded = SockAddr::encode(&address).unwrap();
//! assert_eq!(encoded.decode().unwrap(), address);
//! ```

use std::ffi::OsStr;
use std::fmt;
use std::net::{Ipv4Addr, Ipv6Addr, SocketAddr, SocketAddrV4, SocketAddrV6};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};

use crate::{Error, ErrorKind, Result};

/// An Internet or filesystem Unix-domain socket address.
///
/// Abstract Unix addresses are not supported. An unnamed Unix socket (for
/// example the peer of a `socketpair`) is represented by an empty path.
///
/// Addresses are totally ordered (Internet before Unix, then by address) so
/// they can key ordered maps such as connection pools.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub enum SocketAddress {
    /// An IPv4 or IPv6 address.
    Inet(SocketAddr),
    /// A filesystem Unix-domain socket path.
    Unix(PathBuf),
}

impl SocketAddress {
    /// The Internet address, if this is one.
    pub fn as_inet(&self) -> Option<SocketAddr> {
        match self {
            Self::Inet(address) => Some(*address),
            Self::Unix(_) => None,
        }
    }

    /// The Unix socket path, if this is one.
    pub fn as_unix(&self) -> Option<&Path> {
        match self {
            Self::Unix(path) => Some(path),
            Self::Inet(_) => None,
        }
    }

    /// The address family (`AF_INET`, `AF_INET6` or `AF_UNIX`).
    pub fn family(&self) -> i32 {
        match self {
            Self::Inet(SocketAddr::V4(_)) => libc::AF_INET,
            Self::Inet(SocketAddr::V6(_)) => libc::AF_INET6,
            Self::Unix(_) => libc::AF_UNIX,
        }
    }
}

impl From<SocketAddr> for SocketAddress {
    fn from(address: SocketAddr) -> Self {
        Self::Inet(address)
    }
}

impl From<SocketAddrV4> for SocketAddress {
    fn from(address: SocketAddrV4) -> Self {
        Self::Inet(address.into())
    }
}

impl From<SocketAddrV6> for SocketAddress {
    fn from(address: SocketAddrV6) -> Self {
        Self::Inet(address.into())
    }
}

impl From<PathBuf> for SocketAddress {
    fn from(path: PathBuf) -> Self {
        Self::Unix(path)
    }
}

impl fmt::Display for SocketAddress {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Inet(address) => address.fmt(f),
            Self::Unix(path) => write!(f, "unix:{}", path.display()),
        }
    }
}

/// A `sockaddr` encoding with a stable address.
///
/// The storage lives in a heap allocation that does not move when the
/// `SockAddr` is moved, so [`as_ptr`](SockAddr::as_ptr) may be handed to the
/// kernel for the duration of an operation that owns this value.
#[derive(Clone)]
pub struct SockAddr {
    // A Vec rather than a Box: moving a Box retags its allocation and would
    // invalidate pointers the kernel (or Miri) still holds.
    storage: Vec<Storage>,
}

#[derive(Clone, Copy)]
#[repr(C)]
struct Storage {
    address: libc::sockaddr_storage,
    len: libc::socklen_t,
}

impl SockAddr {
    /// Zeroed storage of maximum size, suitable as an output buffer for
    /// `accept`, `getsockname` or `getpeername`.
    pub fn empty() -> Self {
        Self {
            storage: vec![Storage {
                // SAFETY: all-zero bytes are a valid `sockaddr_storage`.
                address: unsafe { std::mem::zeroed() },
                len: std::mem::size_of::<libc::sockaddr_storage>() as libc::socklen_t,
            }],
        }
    }

    /// Encode `address`.
    ///
    /// Fails with [`ErrorKind::InvalidInput`] for an empty Unix path, a path
    /// containing a NUL byte, or a path too long for `sun_path`.
    pub fn encode(address: &SocketAddress) -> Result<Self> {
        let mut out = Self::empty();
        let len = match address {
            SocketAddress::Inet(SocketAddr::V4(address)) => {
                let value = libc::sockaddr_in {
                    sin_family: libc::AF_INET as libc::sa_family_t,
                    sin_port: address.port().to_be(),
                    sin_addr: libc::in_addr {
                        s_addr: u32::from_ne_bytes(address.ip().octets()),
                    },
                    sin_zero: [0; 8],
                };
                out.write(value)
            }
            SocketAddress::Inet(SocketAddr::V6(address)) => {
                let value = libc::sockaddr_in6 {
                    sin6_family: libc::AF_INET6 as libc::sa_family_t,
                    sin6_port: address.port().to_be(),
                    // std stores flowinfo in network byte order already and
                    // passes it through unchanged; so do we.
                    sin6_flowinfo: address.flowinfo(),
                    sin6_addr: libc::in6_addr {
                        s6_addr: address.ip().octets(),
                    },
                    sin6_scope_id: address.scope_id(),
                };
                out.write(value)
            }
            SocketAddress::Unix(path) => {
                let bytes = path.as_os_str().as_bytes();
                // SAFETY: all-zero bytes are a valid `sockaddr_un`.
                let mut value: libc::sockaddr_un = unsafe { std::mem::zeroed() };
                if bytes.is_empty() || bytes.contains(&0) || bytes.len() >= value.sun_path.len() {
                    return Err(Error::new(ErrorKind::InvalidInput)
                        .with_context("invalid unix socket path"));
                }
                value.sun_family = libc::AF_UNIX as libc::sa_family_t;
                for (dst, src) in value.sun_path.iter_mut().zip(bytes) {
                    *dst = *src as libc::c_char;
                }
                out.write(value);
                std::mem::offset_of!(libc::sockaddr_un, sun_path) + bytes.len() + 1
            }
        };
        out.storage[0].len = len as libc::socklen_t;
        Ok(out)
    }

    fn write<T>(&mut self, value: T) -> usize {
        const { assert!(std::mem::size_of::<T>() <= std::mem::size_of::<libc::sockaddr_storage>()) };
        // SAFETY: storage holds one sockaddr_storage, which is large enough
        // and suitably aligned for every sockaddr type.
        unsafe {
            std::ptr::addr_of_mut!((*self.storage.as_mut_ptr()).address)
                .cast::<T>()
                .write(value)
        };
        std::mem::size_of::<T>()
    }

    /// Pointer to the encoded `sockaddr`, stable across moves.
    pub fn as_ptr(&self) -> *const libc::sockaddr {
        // SAFETY: storage always holds exactly one element.
        unsafe { std::ptr::addr_of!((*self.storage.as_ptr()).address).cast() }
    }

    /// Mutable pointer to the storage, stable across moves.
    pub fn as_mut_ptr(&mut self) -> *mut libc::sockaddr {
        // SAFETY: storage always holds exactly one element.
        unsafe { std::ptr::addr_of_mut!((*self.storage.as_mut_ptr()).address).cast() }
    }

    /// Length of the encoded address in bytes.
    pub fn len(&self) -> libc::socklen_t {
        self.storage[0].len
    }

    /// Whether the encoded length is zero.
    pub fn is_empty(&self) -> bool {
        self.storage[0].len == 0
    }

    /// Pointer to the length, for syscalls that update it in place
    /// (`accept`, `getsockname`). Stable across moves, like
    /// [`as_mut_ptr`](SockAddr::as_mut_ptr).
    pub fn len_mut_ptr(&mut self) -> *mut libc::socklen_t {
        // SAFETY: storage always holds exactly one element.
        unsafe { std::ptr::addr_of_mut!((*self.storage.as_mut_ptr()).len) }
    }

    /// Set the encoded length after the kernel filled the storage.
    ///
    /// Lengths larger than the storage are clamped.
    pub fn set_len(&mut self, len: libc::socklen_t) {
        self.storage[0].len =
            len.min(std::mem::size_of::<libc::sockaddr_storage>() as libc::socklen_t);
    }

    /// Decode the stored address.
    ///
    /// Fails with [`ErrorKind::Unsupported`] for unknown families and abstract
    /// Unix addresses, and [`ErrorKind::InvalidInput`] for truncated data.
    pub fn decode(&self) -> Result<SocketAddress> {
        let storage = &self.storage[0].address;
        let len = self.storage[0].len as usize;
        let invalid = || Error::new(ErrorKind::InvalidInput).with_context("truncated sockaddr");
        if len < std::mem::size_of::<libc::sa_family_t>() {
            return Err(invalid());
        }
        match i32::from(storage.ss_family) {
            libc::AF_INET => {
                if len < std::mem::size_of::<libc::sockaddr_in>() {
                    return Err(invalid());
                }
                // SAFETY: the family and length identify a sockaddr_in, and
                // storage is suitably aligned for it.
                let value = unsafe { &*self.as_ptr().cast::<libc::sockaddr_in>() };
                let ip = Ipv4Addr::from(value.sin_addr.s_addr.to_ne_bytes());
                Ok(SocketAddrV4::new(ip, u16::from_be(value.sin_port)).into())
            }
            libc::AF_INET6 => {
                if len < std::mem::size_of::<libc::sockaddr_in6>() {
                    return Err(invalid());
                }
                // SAFETY: the family and length identify a sockaddr_in6.
                let value = unsafe { &*self.as_ptr().cast::<libc::sockaddr_in6>() };
                Ok(SocketAddrV6::new(
                    Ipv6Addr::from(value.sin6_addr.s6_addr),
                    u16::from_be(value.sin6_port),
                    value.sin6_flowinfo,
                    value.sin6_scope_id,
                )
                .into())
            }
            libc::AF_UNIX => {
                let offset = std::mem::offset_of!(libc::sockaddr_un, sun_path);
                // SAFETY: sockaddr_un fits in sockaddr_storage and every byte
                // of the zero-initialized storage is initialized.
                let value = unsafe { &*self.as_ptr().cast::<libc::sockaddr_un>() };
                let path_len = len.saturating_sub(offset).min(value.sun_path.len());
                let raw: Vec<u8> = value.sun_path[..path_len]
                    .iter()
                    .map(|&c| c as u8)
                    .collect();
                if raw.first() == Some(&0) && raw.len() > 1 {
                    return Err(Error::new(ErrorKind::Unsupported)
                        .with_context("abstract unix socket address"));
                }
                let end = raw.iter().position(|&b| b == 0).unwrap_or(raw.len());
                Ok(SocketAddress::Unix(PathBuf::from(OsStr::from_bytes(
                    &raw[..end],
                ))))
            }
            _ => Err(Error::new(ErrorKind::Unsupported).with_context("unknown address family")),
        }
    }
}

impl fmt::Debug for SockAddr {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SockAddr")
            .field("address", &self.decode())
            .field("len", &self.len())
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn addresses_are_totally_ordered() {
        let v4: SocketAddress = "127.0.0.1:80".parse::<SocketAddr>().unwrap().into();
        let v4_high: SocketAddress = "127.0.0.1:81".parse::<SocketAddr>().unwrap().into();
        let unix = SocketAddress::Unix(PathBuf::from("/a"));
        let mut addresses = vec![unix.clone(), v4_high.clone(), v4.clone()];
        addresses.sort();
        assert_eq!(addresses, [v4, v4_high, unix]);
    }

    fn round_trip(address: SocketAddress) {
        let encoded = SockAddr::encode(&address).unwrap();
        assert_eq!(encoded.decode().unwrap(), address);
    }

    #[test]
    fn inet_round_trip() {
        round_trip("127.0.0.1:80".parse::<SocketAddr>().unwrap().into());
        round_trip("[::1]:443".parse::<SocketAddr>().unwrap().into());
        round_trip(SocketAddrV6::new(Ipv6Addr::LOCALHOST, 9, 0x1234_5678, 3).into());
        round_trip(PathBuf::from("/tmp/sock").into());
    }

    #[test]
    fn ipv6_flowinfo_matches_std() {
        // std passes flowinfo through without byte swapping; compare with
        // what the kernel reports for a std-bound socket would be overkill,
        // so compare the raw field with std's own value.
        let address = SocketAddrV6::new(Ipv6Addr::LOCALHOST, 1, 0x0001_0203, 0);
        let encoded = SockAddr::encode(&address.into()).unwrap();
        // SAFETY: encoded holds a sockaddr_in6.
        let raw = unsafe { &*encoded.as_ptr().cast::<libc::sockaddr_in6>() };
        assert_eq!(raw.sin6_flowinfo, address.flowinfo());
        assert_eq!(raw.sin6_port, 1u16.to_be());
    }

    #[test]
    #[cfg_attr(miri, ignore = "socket syscalls are not supported by Miri")]
    fn ipv4_encoding_matches_kernel() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let mut out = SockAddr::empty();
        // SAFETY: out provides writable storage and a length pointer.
        let rc = unsafe {
            libc::getsockname(
                std::os::fd::AsRawFd::as_raw_fd(&listener),
                out.as_mut_ptr(),
                out.len_mut_ptr(),
            )
        };
        assert_eq!(rc, 0);
        assert_eq!(out.decode().unwrap(), SocketAddress::Inet(address));
        let encoded = SockAddr::encode(&address.into()).unwrap();
        assert_eq!(encoded.len(), out.len());
    }

    #[test]
    fn invalid_unix_paths() {
        for path in ["", "a\0b"] {
            let error = SockAddr::encode(&PathBuf::from(path).into()).unwrap_err();
            assert_eq!(error.kind(), ErrorKind::InvalidInput);
        }
        let long = "x".repeat(108);
        assert!(SockAddr::encode(&PathBuf::from(long).into()).is_err());
        assert!(SockAddr::encode(&PathBuf::from("y".repeat(107)).into()).is_ok());
    }

    #[test]
    #[cfg_attr(miri, ignore = "socket syscalls are not supported by Miri")]
    fn decode_edge_cases() {
        let mut empty = SockAddr::empty();
        assert_eq!(
            empty.decode().unwrap_err().kind(),
            ErrorKind::Unsupported,
            "AF_UNSPEC"
        );
        empty.set_len(0);
        assert!(empty.is_empty());
        assert_eq!(empty.decode().unwrap_err().kind(), ErrorKind::InvalidInput);

        let mut truncated =
            SockAddr::encode(&"[::1]:1".parse::<SocketAddr>().unwrap().into()).unwrap();
        truncated.set_len(8);
        assert_eq!(
            truncated.decode().unwrap_err().kind(),
            ErrorKind::InvalidInput
        );
        truncated.set_len(u32::MAX);
        assert_eq!(
            truncated.len() as usize,
            std::mem::size_of::<libc::sockaddr_storage>()
        );

        let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
        let mut out = SockAddr::empty();
        // SAFETY: out provides writable storage and a length pointer.
        let rc = unsafe {
            libc::getsockname(
                std::os::fd::AsRawFd::as_raw_fd(&a),
                out.as_mut_ptr(),
                out.len_mut_ptr(),
            )
        };
        assert_eq!(rc, 0);
        assert_eq!(out.decode().unwrap(), SocketAddress::Unix(PathBuf::new()));
    }

    #[test]
    fn pointer_is_stable_across_moves() {
        let encoded = SockAddr::encode(&"1.2.3.4:5".parse::<SocketAddr>().unwrap().into()).unwrap();
        let ptr = encoded.as_ptr();
        let moved = Box::new(encoded);
        assert_eq!(moved.as_ptr(), ptr);
    }

    #[test]
    fn accessors_and_display() {
        let inet = SocketAddress::from("10.0.0.1:1".parse::<SocketAddr>().unwrap());
        assert_eq!(inet.family(), libc::AF_INET);
        assert!(inet.as_unix().is_none());
        assert_eq!(inet.to_string(), "10.0.0.1:1");
        let unix = SocketAddress::from(PathBuf::from("/s"));
        assert_eq!(unix.family(), libc::AF_UNIX);
        assert_eq!(unix.as_unix(), Some(Path::new("/s")));
        assert!(unix.as_inet().is_none());
        assert_eq!(unix.to_string(), "unix:/s");
    }
}
