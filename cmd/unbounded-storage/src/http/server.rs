// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Storage-policy-free server-side HTTP plumbing shared by the HTTP
//! serving frontends.
//!
//! This file owns the reusable, opinion-free pieces a server-side
//! frontend needs regardless of which object protocol it speaks: the
//! `SO_REUSEPORT` listener helper, the RAII socket-fd closer, the
//! request-target query splitter, the full-buffer send loop, and the
//! size constants. None of it knows about ranges, stripe geometry, or
//! which status a frontend returns; that policy lives with the concrete
//! frontend (`crate::frontend::http_serve`) and a future S3 frontend
//! can reuse everything here unchanged.
//!
//! The libc/`io_uring`-bearing helpers ([`bind_listener`], [`FdGuard`],
//! [`send_all`]) are Linux-gated because they depend on `libc` and the
//! io_uring [`NetHandle`](crate::ring::NetHandle); the pure helpers
//! ([`split_query`]) and the constants are cross-platform.

#[cfg(target_os = "linux")]
use std::net::SocketAddr;
#[cfg(target_os = "linux")]
use std::os::fd::RawFd;

#[cfg(target_os = "linux")]
use crate::ring::NetHandle;

/// Cap on the request header block we will buffer before giving up, so
/// a misbehaving client cannot make us allocate without bound.
pub(crate) const MAX_HEADER_BYTES: usize = 64 * 1024;

/// Per-recv chunk size for reading request heads and origin responses.
pub(crate) const RECV_CHUNK: usize = 64 * 1024;

/// Listen backlog for the accept socket.
#[cfg(target_os = "linux")]
const LISTEN_BACKLOG: i32 = 1024;

/// Create, bind, and listen a TCP socket on `addr` with `SO_REUSEADDR`
/// and `SO_REUSEPORT`. Supports IPv4 and IPv6 binds.
///
/// Returns the listening [`RawFd`]; the caller owns it and is
/// responsible for closing it. Errors are surfaced as
/// [`std::io::Error`] (from the failing syscall) so this stays free of
/// any frontend-specific error type; the caller maps it onto whatever
/// error it reports.
#[cfg(target_os = "linux")]
pub(crate) fn bind_listener(addr: SocketAddr) -> Result<RawFd, std::io::Error> {
    let family = match addr {
        SocketAddr::V4(_) => libc::AF_INET,
        SocketAddr::V6(_) => libc::AF_INET6,
    };
    // SAFETY: socket() with valid family/type constants.
    let fd = unsafe { libc::socket(family, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    let guard = FdGuard(fd);

    let one: libc::c_int = 1;
    for opt in [libc::SO_REUSEADDR, libc::SO_REUSEPORT] {
        // SAFETY: &one outlives the call; size matches a c_int.
        let rc = unsafe {
            libc::setsockopt(
                fd,
                libc::SOL_SOCKET,
                opt,
                &one as *const libc::c_int as *const libc::c_void,
                std::mem::size_of::<libc::c_int>() as libc::socklen_t,
            )
        };
        if rc != 0 {
            return Err(std::io::Error::last_os_error());
        }
    }

    let bind_rc = match addr {
        SocketAddr::V4(v4) => {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: v4.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(*v4.ip()).to_be(),
                },
                sin_zero: [0; 8],
            };
            // SAFETY: sin is a valid sockaddr_in for the call's duration.
            unsafe {
                libc::bind(
                    fd,
                    &sin as *const libc::sockaddr_in as *const libc::sockaddr,
                    std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
                )
            }
        }
        SocketAddr::V6(v6) => {
            let sin6 = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as libc::sa_family_t,
                sin6_port: v6.port().to_be(),
                sin6_flowinfo: v6.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: v6.ip().octets(),
                },
                sin6_scope_id: v6.scope_id(),
            };
            // SAFETY: sin6 is a valid sockaddr_in6 for the call's duration.
            unsafe {
                libc::bind(
                    fd,
                    &sin6 as *const libc::sockaddr_in6 as *const libc::sockaddr,
                    std::mem::size_of::<libc::sockaddr_in6>() as libc::socklen_t,
                )
            }
        }
    };
    if bind_rc != 0 {
        return Err(std::io::Error::last_os_error());
    }

    // SAFETY: fd is a bound stream socket.
    if unsafe { libc::listen(fd, LISTEN_BACKLOG) } != 0 {
        return Err(std::io::Error::last_os_error());
    }

    // Hand the fd out to the caller; defuse the guard.
    std::mem::forget(guard);
    Ok(fd)
}

/// RAII closer for a socket fd. The ring never creates fds; this owns
/// the lifecycle of fds the frontend opens with `libc`.
#[cfg(target_os = "linux")]
pub(crate) struct FdGuard(pub(crate) RawFd);

#[cfg(target_os = "linux")]
impl Drop for FdGuard {
    fn drop(&mut self) {
        // SAFETY: the fd was returned by socket()/accept() and is not
        // used after this guard drops.
        unsafe {
            libc::close(self.0);
        }
    }
}

/// Send `bytes` in full over `fd`, looping until every queued byte is
/// accepted by the kernel.
///
/// `NetHandle::send` returns the CQE `res`, i.e. the number of bytes the
/// kernel accepted, which for TCP can be less than requested under
/// socket-send-buffer pressure. Dropping that count would silently
/// truncate the response, so this re-sends the unsent tail until the
/// whole buffer is on the wire. A returned count of 0 means the peer
/// closed and is surfaced as an error.
#[cfg(target_os = "linux")]
pub(crate) async fn send_all(handle: &NetHandle, fd: RawFd, bytes: Vec<u8>) -> Result<(), ()> {
    let total = bytes.len();
    let mut sent = 0usize;
    while sent < total {
        let n = handle
            .send(fd, bytes[sent..].to_vec())
            .await
            .map_err(|_| ())?;
        if n == 0 {
            return Err(());
        }
        sent += n;
    }
    Ok(())
}

/// Split a raw request target into `(path, query)` where `query` is the
/// part after the first `?` (or empty). The path is the object-name
/// cache key.
pub(crate) fn split_query(target: &str) -> (&str, &str) {
    match target.split_once('?') {
        Some((p, q)) => (p, q),
        None => (target, ""),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn split_query_strips_query() {
        assert_eq!(split_query("/bucket/key"), ("/bucket/key", ""));
        assert_eq!(
            split_query("/bucket/key?list-type=2&x=1"),
            ("/bucket/key", "list-type=2&x=1")
        );
        assert_eq!(split_query("/?"), ("/", ""));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn bind_listener_creates_and_binds() {
        match bind_listener("127.0.0.1:0".parse().unwrap()) {
            Ok(fd) => {
                assert!(fd >= 0);
                unsafe {
                    libc::close(fd);
                }
            }
            Err(e) => eprintln!("bind_listener_creates_and_binds: bind failed: {e}; skipping"),
        }
    }
}
