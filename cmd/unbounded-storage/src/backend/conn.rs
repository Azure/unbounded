// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Reusable origin TCP connections for HTTP-family backends.

use std::collections::HashMap;
use std::os::fd::RawFd;
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::thread::ThreadId;

use crate::bufferpool::Error;
use crate::http::{ResponseHead, StatusCode, response_keep_alive};
use crate::ring::{NetHandle, SockAddr};
use crate::tls::TlsContext;

#[derive(Clone)]
pub(super) struct OriginConnPool {
    inner: Arc<Mutex<PoolInner>>,
}

struct PoolInner {
    max_idle_per_ring: usize,
    idle: HashMap<PoolKey, Vec<TcpConn>>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
struct PoolKey {
    thread: ThreadId,
    ring: usize,
}

impl OriginConnPool {
    pub(super) fn new(max_idle_per_ring: usize) -> Self {
        Self {
            inner: Arc::new(Mutex::new(PoolInner {
                max_idle_per_ring: max_idle_per_ring.max(1),
                idle: HashMap::new(),
            })),
        }
    }

    pub(super) fn checkout(&self, handle: &NetHandle) -> Option<TcpConn> {
        let key = pool_key(handle);
        loop {
            let conn = self
                .inner
                .lock()
                .expect("origin connection pool poisoned")
                .idle
                .get_mut(&key)
                .and_then(Vec::pop)?;
            if tcp_conn_is_reusable(conn.fd()) {
                return Some(conn);
            }
        }
    }

    pub(super) fn put(&self, handle: &NetHandle, conn: TcpConn) {
        let key = pool_key(handle);
        let mut inner = self.inner.lock().expect("origin connection pool poisoned");
        let max_idle = inner.max_idle_per_ring;
        let idle = inner.idle.entry(key).or_default();
        if idle.len() < max_idle {
            idle.push(conn);
        }
    }
}

fn pool_key(handle: &NetHandle) -> PoolKey {
    PoolKey {
        thread: std::thread::current().id(),
        ring: handle.ring_key(),
    }
}

pub(super) struct TcpConn {
    fd: RawFd,
    tls: bool,
}

pub(super) struct OriginResponseHead {
    pub(super) status: StatusCode,
    pub(super) version_minor: u8,
    pub(super) header_end: usize,
    pub(super) content_length: Option<u64>,
    pub(super) content_range_start: Option<u64>,
    pub(super) cache_control: Option<String>,
    pub(super) connection: Option<String>,
    pub(super) buf: Vec<u8>,
}

struct OriginConn {
    conn: TcpConn,
    reused: bool,
}

impl TcpConn {
    pub(super) fn open() -> Result<Self, Error> {
        // SAFETY: socket() with valid AF/type/protocol constants.
        let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
        if fd < 0 {
            return Err(io_to_err(std::io::Error::last_os_error()));
        }
        Ok(Self { fd, tls: false })
    }

    pub(super) fn fd(&self) -> RawFd {
        self.fd
    }

    pub(super) fn is_tls(&self) -> bool {
        self.tls
    }
}

impl Drop for TcpConn {
    fn drop(&mut self) {
        // SAFETY: fd was returned by socket() and is not used after this.
        unsafe {
            libc::close(self.fd);
        }
    }
}

async fn checkout_or_connect(
    pool: &OriginConnPool,
    handle: &NetHandle,
    origin: SockAddr,
    tls: &Option<Rc<TlsContext>>,
    sni_host: &str,
) -> Result<OriginConn, Error> {
    if let Some(conn) = pool.checkout(handle) {
        return Ok(OriginConn { conn, reused: true });
    }

    let mut conn = TcpConn::open()?;
    handle.connect(conn.fd(), origin).await.map_err(io_to_err)?;
    conn.tls = crate::tls::maybe_handshake(tls, handle, conn.fd(), sni_host).await?;
    Ok(OriginConn {
        conn,
        reused: false,
    })
}

pub(super) async fn send_request_read_head(
    pool: &OriginConnPool,
    handle: &NetHandle,
    origin: SockAddr,
    tls: &Option<Rc<TlsContext>>,
    sni_host: &str,
    request: Vec<u8>,
    max_head: Option<usize>,
    malformed_msg: &'static str,
    closed_msg: &'static str,
    too_large_msg: &'static str,
) -> Result<(TcpConn, OriginResponseHead), Error> {
    let mut stale_retry = true;

    'connect: loop {
        let origin_conn = checkout_or_connect(pool, handle, origin, tls, sni_host).await?;
        let conn = origin_conn.conn;
        let reused = origin_conn.reused;
        let fd = conn.fd();
        let is_tls = conn.is_tls();

        match handle.send(fd, request.clone()).await {
            Ok(_) => {}
            Err(e) if reused && stale_retry => {
                stale_retry = false;
                continue;
            }
            Err(e) => return Err(io_to_err(e)),
        }

        let mut buf: Vec<u8> = Vec::new();
        loop {
            if let Some(h) = ResponseHead::parse(&buf).map_err(|_| Error::from(malformed_msg))? {
                let head = OriginResponseHead {
                    status: h.status,
                    version_minor: h.version_minor,
                    header_end: h.header_end,
                    content_length: h.content_length(),
                    content_range_start: h.content_range_start(),
                    cache_control: h.header("cache-control").map(ToOwned::to_owned),
                    connection: h.header("connection").map(ToOwned::to_owned),
                    buf,
                };
                return Ok((conn, head));
            }

            if max_head.is_some_and(|max| buf.len() >= max) {
                return Err(Error::from(too_large_msg));
            }

            match crate::tls::recv_chunk(handle, fd, is_tls).await {
                Ok(chunk) if chunk.is_empty() && reused && stale_retry && buf.is_empty() => {
                    stale_retry = false;
                    continue 'connect;
                }
                Ok(chunk) if chunk.is_empty() => return Err(Error::from(closed_msg)),
                Ok(chunk) => buf.extend_from_slice(&chunk),
                Err(_) if reused && stale_retry && buf.is_empty() => {
                    stale_retry = false;
                    continue 'connect;
                }
                Err(e) => return Err(e),
            }
        }
    }
}

pub(super) fn body_response_reusable(
    version_minor: u8,
    connection: Option<&str>,
    content_length: Option<u64>,
    body_cap: usize,
    leading_len: usize,
    filled: usize,
) -> bool {
    if !response_keep_alive(version_minor, connection)
        || leading_len > body_cap
        || filled != body_cap
    {
        return false;
    }

    content_length.and_then(|n| usize::try_from(n).ok()) == Some(body_cap)
}

pub(super) fn head_response_reusable(
    version_minor: u8,
    connection: Option<&str>,
    header_end: usize,
    received: usize,
) -> bool {
    response_keep_alive(version_minor, connection) && header_end == received
}

fn tcp_conn_is_reusable(fd: RawFd) -> bool {
    let mut byte = [0u8; 1];
    loop {
        // SAFETY: `byte` is valid for one byte and `fd` is owned by the
        // connection being considered for reuse. MSG_DONTWAIT avoids
        // blocking the shard thread; MSG_PEEK leaves any byte in place.
        let n = unsafe {
            libc::recv(
                fd,
                byte.as_mut_ptr().cast(),
                byte.len(),
                libc::MSG_PEEK | libc::MSG_DONTWAIT,
            )
        };
        if n == 0 {
            return false;
        }
        if n > 0 {
            return false;
        }
        let err = std::io::Error::last_os_error();
        match err.raw_os_error() {
            Some(code) if code == libc::EINTR => continue,
            Some(code) if code == libc::EAGAIN || code == libc::EWOULDBLOCK => return true,
            _ => return false,
        }
    }
}

fn io_to_err(e: std::io::Error) -> Error {
    match e.raw_os_error() {
        Some(code) => Error::Io(code),
        None => Error::transport(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::response_closes_after_body;

    #[test]
    fn body_reuse_requires_known_fully_consumed_body() {
        assert!(body_response_reusable(1, None, Some(4096), 4096, 0, 4096));
        assert!(body_response_reusable(
            1,
            Some("keep-alive"),
            Some(100),
            100,
            10,
            100
        ));
        assert!(!body_response_reusable(
            1,
            Some("keep-alive, close"),
            Some(4096),
            4096,
            0,
            4096
        ));
        assert!(!body_response_reusable(1, None, None, 4096, 0, 4096));
        assert!(!body_response_reusable(1, None, Some(8192), 4096, 0, 4096));
        assert!(!body_response_reusable(
            1,
            None,
            Some(4096),
            4096,
            4097,
            4096
        ));
    }

    #[test]
    fn head_reuse_rejects_close_and_extra_bytes() {
        assert!(head_response_reusable(1, None, 42, 42));
        assert!(head_response_reusable(1, Some("keep-alive"), 42, 42));
        assert!(!head_response_reusable(1, Some("close"), 42, 42));
        assert!(!head_response_reusable(1, None, 42, 43));
    }

    #[test]
    fn response_reuse_follows_http_version_rules() {
        assert!(body_response_reusable(1, None, Some(1), 1, 0, 1));
        assert!(!body_response_reusable(1, Some("close"), Some(1), 1, 0, 1));
        assert!(!body_response_reusable(0, None, Some(1), 1, 0, 1));
        assert!(body_response_reusable(
            0,
            Some("keep-alive"),
            Some(1),
            1,
            0,
            1
        ));
    }

    #[test]
    fn close_delimited_bodies_require_close_semantics() {
        assert!(!response_closes_after_body(1, None));
        assert!(response_closes_after_body(1, Some("close")));
        assert!(response_closes_after_body(0, None));
        assert!(!response_closes_after_body(0, Some("keep-alive")));
    }
}
