// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Validated numeric TCP and filesystem Unix socket destinations.

use std::{fmt, io, net::SocketAddr};

#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
pub enum Address {
    Tcp(SocketAddr),
    Unix(UnixPath),
}

#[derive(Clone, Copy, Eq, PartialEq, Ord, PartialOrd, Hash)]
pub struct UnixPath {
    bytes: [u8; 108],
    len: u8,
}

impl UnixPath {
    pub fn new(path: &str) -> io::Result<Self> {
        if !path.starts_with('/') || path.len() > 107 || path.as_bytes().contains(&0) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "Unix socket path must be absolute, NUL-free, and at most 107 bytes",
            ));
        }
        let mut bytes = [0; 108];
        bytes[..path.len()].copy_from_slice(path.as_bytes());
        Ok(Self {
            bytes,
            len: path.len() as u8,
        })
    }

    pub fn as_str(&self) -> &str {
        // Construction copies a valid UTF-8 string and the fields are private.
        std::str::from_utf8(&self.bytes[..self.len as usize]).unwrap()
    }

    pub(crate) fn sockaddr(&self) -> libc::sockaddr_un {
        // SAFETY: zero initializes the address including its terminating NUL.
        let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
        address.sun_family = libc::AF_UNIX as _;
        for (out, input) in address.sun_path.iter_mut().zip(self.bytes) {
            *out = input as libc::c_char;
        }
        address
    }

    pub(crate) fn sockaddr_len(&self) -> usize {
        std::mem::offset_of!(libc::sockaddr_un, sun_path) + self.len as usize + 1
    }
}

impl fmt::Debug for UnixPath {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.as_str().fmt(f)
    }
}

impl From<SocketAddr> for Address {
    fn from(address: SocketAddr) -> Self {
        Self::Tcp(address)
    }
}

impl Address {
    pub fn unix(path: &str) -> io::Result<Self> {
        UnixPath::new(path).map(Self::Unix)
    }

    pub fn tcp(self) -> Option<SocketAddr> {
        match self {
            Self::Tcp(address) => Some(address),
            Self::Unix(_) => None,
        }
    }

    pub(crate) fn domain(self) -> libc::c_int {
        match self {
            Self::Tcp(SocketAddr::V4(_)) => libc::AF_INET,
            Self::Tcp(SocketAddr::V6(_)) => libc::AF_INET6,
            Self::Unix(_) => libc::AF_UNIX,
        }
    }
}

impl fmt::Display for Address {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Tcp(address) => address.fmt(f),
            Self::Unix(path) => f.write_str(path.as_str()),
        }
    }
}

#[cfg(test)]
#[path = "../tests/http/socket.rs"]
mod tests;
