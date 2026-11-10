// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Zero-copy forwarding of object payload from the Racer Unix socket to the
//! client TCP socket with splice(2) through a pipe.
//!
//! Payload never enters userspace: the kernel moves socket buffer pages into
//! the pipe and from the pipe into the TCP send queue. Both sockets are
//! nonblocking and driven by tokio's epoll readiness.

use crate::racer::BODY_TIMEOUT;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use tokio::io::Interest;
use tokio::net::{TcpStream, UnixStream};

/// Requested pipe capacity. Larger pipes mean fewer splice calls per byte.
/// The kernel may refuse (pipe-max-size or per-user limits); the default
/// 64 KiB pipe still works, just with more calls.
const PIPE_SIZE: libc::c_int = 1 << 20;

/// A nonblocking pipe used as the splice buffer for one connection.
pub struct Pipe {
    read: OwnedFd,
    write: OwnedFd,
    capacity: usize,
}

impl Pipe {
    pub fn new() -> io::Result<Self> {
        let mut fds = [0 as RawFd; 2];
        // SAFETY: `fds` is a valid two-element array for pipe2 to fill.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) } != 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: pipe2 succeeded, so both descriptors are open and owned here.
        let (read, write) = unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };
        // SAFETY: fcntl on a descriptor we own; failure leaves the default size.
        let set = unsafe { libc::fcntl(write.as_raw_fd(), libc::F_SETPIPE_SZ, PIPE_SIZE) };
        // SAFETY: as above, F_GETPIPE_SZ only reads the pipe size.
        let capacity = if set > 0 {
            set
        } else {
            unsafe { libc::fcntl(write.as_raw_fd(), libc::F_GETPIPE_SZ) }
        };
        Ok(Self {
            read,
            write,
            capacity: usize::try_from(capacity).unwrap_or(0).max(4096),
        })
    }

    pub fn capacity(&self) -> usize {
        self.capacity
    }
}

fn splice(from: RawFd, to: RawFd, len: usize, more: bool) -> io::Result<usize> {
    let mut flags = libc::SPLICE_F_MOVE | libc::SPLICE_F_NONBLOCK;
    if more {
        flags |= libc::SPLICE_F_MORE;
    }
    loop {
        // SAFETY: both descriptors are open for the duration of the call and
        // null offsets are required for sockets and pipes.
        let n = unsafe {
            libc::splice(
                from,
                std::ptr::null_mut(),
                to,
                std::ptr::null_mut(),
                len,
                flags,
            )
        };
        if n >= 0 {
            return Ok(n as usize);
        }
        let e = io::Error::last_os_error();
        if e.kind() != io::ErrorKind::Interrupted {
            return Err(e);
        }
    }
}

/// Which side failed. Either way the response is already committed and the
/// client connection must be aborted, but tests and metrics care.
#[derive(Debug, Eq, PartialEq)]
pub enum SpliceError {
    /// Racer closed, reset, or stalled before the payload was complete.
    Source,
    /// The client stopped accepting data.
    Destination,
}

/// Move exactly `len` bytes from `src` to `dst`. The caller must ensure no
/// payload is buffered in userspace and that `pipe` is empty.
///
/// The pipe is filled once, then drained completely, so a full pipe is
/// never confused with an empty socket: a nonblocking splice into a pipe
/// returns EAGAIN for both. Readiness is only consumed (via `try_io`) when
/// the pipe is empty, so EAGAIN then really means "no socket data".
pub async fn forward(
    src: &UnixStream,
    pipe: &Pipe,
    dst: &TcpStream,
    mut len: u64,
    more: bool,
    watch: &mut ClientWatch<'_>,
) -> Result<(), SpliceError> {
    let (src_fd, dst_fd) = (src.as_raw_fd(), dst.as_raw_fd());
    let (pipe_r, pipe_w) = (pipe.read.as_raw_fd(), pipe.write.as_raw_fd());
    while len > 0 {
        let want = usize::try_from(len)
            .unwrap_or(usize::MAX)
            .min(pipe.capacity);
        // Fill: wait for socket data, then move it into the pipe.
        let mut filled = loop {
            match src.try_io(Interest::READABLE, || splice(src_fd, pipe_w, want, true)) {
                Ok(0) => return Err(SpliceError::Source),
                Ok(n) => {
                    // try_io bypasses tokio's coop budget, so charge it here:
                    // with both sockets always ready this loop would otherwise
                    // never yield and starve other connections on the worker.
                    tokio::task::consume_budget().await;
                    break n;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    let wait = tokio::time::timeout(BODY_TIMEOUT, src.readable());
                    match watch.guard(wait).await {
                        Some(Ok(Ok(()))) => {}
                        Some(_) => return Err(SpliceError::Source),
                        None => return Err(SpliceError::Destination),
                    }
                }
                Err(_) => return Err(SpliceError::Source),
            }
        };
        len -= filled as u64;
        // Top up opportunistically without touching readiness: EAGAIN here
        // may only mean the pipe ran out of slots.
        while len > 0 && filled < pipe.capacity {
            let want = usize::try_from(len)
                .unwrap_or(usize::MAX)
                .min(pipe.capacity - filled);
            match splice(src_fd, pipe_w, want, true) {
                Ok(0) => return Err(SpliceError::Source),
                Ok(n) => {
                    filled += n;
                    len -= n as u64;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                Err(_) => return Err(SpliceError::Source),
            }
        }
        // Drain: move everything in the pipe into the TCP socket.
        let more = more || len > 0;
        while filled > 0 {
            match dst.try_io(Interest::WRITABLE, || splice(pipe_r, dst_fd, filled, more)) {
                Ok(0) => return Err(SpliceError::Destination),
                Ok(n) => {
                    filled -= n;
                    tokio::task::consume_budget().await;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    match tokio::time::timeout(BODY_TIMEOUT, dst.writable()).await {
                        Ok(Ok(())) => {}
                        _ => return Err(SpliceError::Destination),
                    }
                }
                Err(_) => return Err(SpliceError::Destination),
            }
        }
    }
    Ok(())
}

/// Watches the client socket while the sidecar waits on Racer, like Go's
/// background read, so a client reset drops the Racer operation and frees
/// the connection slot instead of waiting out a Racer stall.
///
/// Readiness comes from the already-registered socket, so a Racer that is
/// already readable costs no extra syscall. Bytes that arrive are appended
/// to `rbuf` as pipelined input. EOF (a valid half-close) or a buffer at
/// `cap` stops the watch for the rest of this value's lifetime, since the
/// socket then stays readable and selecting on it would spin; a later reset
/// is still caught by the next write.
pub struct ClientWatch<'a> {
    stream: &'a TcpStream,
    rbuf: &'a mut Vec<u8>,
    cap: usize,
    watching: bool,
}

impl<'a> ClientWatch<'a> {
    pub fn new(stream: &'a TcpStream, rbuf: &'a mut Vec<u8>, cap: usize) -> Self {
        Self {
            stream,
            rbuf,
            cap,
            watching: true,
        }
    }

    /// Run `fut`, returning `None` if the client connection failed first.
    pub async fn guard<F: Future>(&mut self, fut: F) -> Option<F::Output> {
        tokio::pin!(fut);
        loop {
            tokio::select! {
                biased;
                out = &mut fut => return Some(out),
                r = self.stream.readable(), if self.watching => {
                    if r.is_err() {
                        return None;
                    }
                    if self.rbuf.len() >= self.cap {
                        self.watching = false;
                        continue;
                    }
                    if self.rbuf.capacity() - self.rbuf.len() < 2048 {
                        self.rbuf.reserve(8192);
                    }
                    match self.stream.try_read_buf(self.rbuf) {
                        Ok(0) => self.watching = false,
                        Ok(_) => {}
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
                        Err(_) => return None,
                    }
                }
            }
        }
    }
}

/// Send `data` on `dst`, hinting with MSG_MORE that payload follows so the
/// head and the first payload bytes share segments.
pub async fn send_more(dst: &TcpStream, mut data: &[u8]) -> io::Result<()> {
    let fd = dst.as_raw_fd();
    while !data.is_empty() {
        let sent = dst.try_io(Interest::WRITABLE, || {
            // SAFETY: `data` is a valid readable buffer for its length and
            // `fd` stays open while `dst` is borrowed.
            let n = unsafe {
                libc::send(
                    fd,
                    data.as_ptr().cast(),
                    data.len(),
                    libc::MSG_MORE | libc::MSG_NOSIGNAL,
                )
            };
            if n < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(n as usize)
            }
        });
        match sent {
            Ok(n) => {
                data = &data[n..];
                // See forward: try_io does not consume the coop budget.
                tokio::task::consume_budget().await;
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                tokio::time::timeout(BODY_TIMEOUT, dst.writable())
                    .await
                    .map_err(|_| io::Error::from(io::ErrorKind::TimedOut))??;
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::future::Future;
    use std::io::Read;
    use std::task::{Context, Poll};

    /// Spend the rest of the coop budget without yielding.
    async fn exhaust_budget() {
        while tokio::task::coop::has_budget_remaining() {
            tokio::task::consume_budget().await;
        }
    }

    async fn poll_once<F: Future>(fut: F) -> Poll<F::Output> {
        let mut fut = std::pin::pin!(fut);
        std::future::poll_fn(|cx: &mut Context<'_>| Poll::Ready(fut.as_mut().poll(cx))).await
    }

    async fn tcp_pair() -> (TcpStream, std::net::TcpStream) {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let a = TcpStream::connect(l.local_addr().unwrap()).await.unwrap();
        let (b, _) = l.accept().unwrap();
        (a, b)
    }

    #[tokio::test(flavor = "current_thread")]
    async fn send_more_yields_when_budget_exhausted() {
        let (dst, mut peer) = tcp_pair().await;
        dst.writable().await.unwrap();
        exhaust_budget().await;
        // The socket is writable, so without a budget charge this would
        // complete without ever returning to the scheduler.
        assert!(poll_once(send_more(&dst, b"x")).await.is_pending());
        send_more(&dst, b"y").await.unwrap();
        let mut b = [0u8; 2];
        peer.read_exact(&mut b).unwrap();
    }

    #[tokio::test(flavor = "current_thread")]
    async fn forward_yields_when_budget_exhausted() {
        let (src, mut feed) = UnixStream::pair().unwrap();
        tokio::io::AsyncWriteExt::write_all(&mut feed, b"hello")
            .await
            .unwrap();
        let (dst, mut peer) = tcp_pair().await;
        let (client, _client_peer) = tcp_pair().await;
        let mut rbuf = Vec::new();
        let mut watch = ClientWatch::new(&client, &mut rbuf, 1024);
        let pipe = Pipe::new().unwrap();
        src.readable().await.unwrap();
        dst.writable().await.unwrap();
        exhaust_budget().await;
        let fwd = forward(&src, &pipe, &dst, 5, false, &mut watch);
        let mut fwd = std::pin::pin!(fwd);
        assert!(poll_once(fwd.as_mut()).await.is_pending());
        fwd.await.unwrap();
        let mut b = [0u8; 5];
        peer.read_exact(&mut b).unwrap();
        assert_eq!(&b, b"hello");
    }
}
