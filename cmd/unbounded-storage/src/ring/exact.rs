// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Exact socket operations for framed TCP protocols.

use std::future::Future;
use std::io;
use std::os::fd::RawFd;

use super::network::{NetHandle, checked_u32_len};

impl NetHandle {
    /// Send the entire heap buffer, retrying short sends.
    pub fn send_exact(
        &self,
        fd: RawFd,
        buf: Vec<u8>,
    ) -> impl Future<Output = io::Result<()>> + 'static {
        let handle = self.clone();
        async move {
            checked_u32_len(buf.len())?;
            let mut sent = 0;
            while sent < buf.len() {
                // `send` owns each submitted allocation until completion.
                // Retaining `buf` lets a short send safely rebuild its tail.
                let n = handle.send(fd, buf[sent..].to_vec()).await?;
                if n == 0 {
                    return Err(io::Error::new(io::ErrorKind::WriteZero, "zero-byte send"));
                }
                sent += n;
            }
            Ok(())
        }
    }

    /// Receive exactly `len` heap bytes, returning `UnexpectedEof` if the
    /// peer closes before the frame is complete.
    pub fn recv_exact(
        &self,
        fd: RawFd,
        len: usize,
    ) -> impl Future<Output = io::Result<Vec<u8>>> + 'static {
        let handle = self.clone();
        async move {
            checked_u32_len(len)?;
            let mut data = Vec::with_capacity(len);
            while data.len() < len {
                let chunk = handle.recv(fd, len - data.len()).await?;
                if chunk.is_empty() {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "socket closed before exact receive completed",
                    ));
                }
                data.extend_from_slice(&chunk);
            }
            Ok(data)
        }
    }

    /// Receive exactly `len` bytes into a registered fixed-buffer range.
    pub fn recv_fixed_exact(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<()>> + 'static {
        let handle = self.clone();
        async move {
            if buf_index != 0 {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            handle
                .ring_cell()
                .fixed_range(buf_index, page_byte_offset, len)?;
            let mut received = 0;
            while received < len {
                let offset = page_byte_offset
                    .checked_add(received)
                    .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
                let n = handle
                    .recv_fixed(fd, buf_index, offset, len - received)
                    .await?;
                if n == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "socket closed before exact fixed receive completed",
                    ));
                }
                received += n;
            }
            Ok(())
        }
    }

    /// SEND_ZC exactly `len` bytes from a registered fixed-buffer range.
    /// Each short send waits for its final notification before advancing.
    pub fn send_zc_fixed_exact(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<()>> + 'static {
        let handle = self.clone();
        async move {
            if buf_index != 0 {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            handle
                .ring_cell()
                .fixed_range(buf_index, page_byte_offset, len)?;
            let mut sent = 0;
            while sent < len {
                let offset = page_byte_offset
                    .checked_add(sent)
                    .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
                let n = handle
                    .send_zc_fixed(fd, buf_index, offset, len - sent)
                    .await?;
                if n == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::WriteZero,
                        "zero-byte SEND_ZC",
                    ));
                }
                sent += n;
            }
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use std::pin::Pin;
    use std::rc::Rc;
    use std::task::{Context, Poll};

    use super::*;
    use crate::ring::NetworkRing;
    use crate::runtime::noop_waker;

    fn socketpair() -> Option<(RawFd, RawFd)> {
        let mut fds = [0; 2];
        let rc = unsafe { libc::socketpair(libc::AF_UNIX, libc::SOCK_STREAM, 0, fds.as_mut_ptr()) };
        (rc == 0).then_some((fds[0], fds[1]))
    }

    fn tcp_loopback_pair() -> Option<(RawFd, RawFd)> {
        unsafe {
            let listener = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if listener < 0 {
                return None;
            }
            let addr = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: 0,
                sin_addr: libc::in_addr {
                    s_addr: u32::to_be(0x7f00_0001),
                },
                sin_zero: [0; 8],
            };
            let addr_len = std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t;
            if libc::bind(
                listener,
                &addr as *const _ as *const libc::sockaddr,
                addr_len,
            ) != 0
                || libc::listen(listener, 1) != 0
            {
                libc::close(listener);
                return None;
            }
            let mut bound: libc::sockaddr_in = std::mem::zeroed();
            let mut bound_len = addr_len;
            if libc::getsockname(
                listener,
                &mut bound as *mut _ as *mut libc::sockaddr,
                &mut bound_len,
            ) != 0
            {
                libc::close(listener);
                return None;
            }
            let client = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if client < 0
                || libc::connect(
                    client,
                    &bound as *const _ as *const libc::sockaddr,
                    bound_len,
                ) != 0
            {
                if client >= 0 {
                    libc::close(client);
                }
                libc::close(listener);
                return None;
            }
            let server = libc::accept(listener, std::ptr::null_mut(), std::ptr::null_mut());
            libc::close(listener);
            if server < 0 {
                libc::close(client);
                return None;
            }
            Some((client, server))
        }
    }

    fn block_on<F: Future>(mut fut: Pin<&mut F>) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        for _ in 0..5_000_000 {
            if let Poll::Ready(value) = fut.as_mut().poll(&mut cx) {
                return value;
            }
        }
        panic!("exact I/O future spun without progress");
    }

    fn is_unsupported(err: &io::Error) -> bool {
        matches!(
            err.raw_os_error(),
            Some(libc::ENOSYS) | Some(libc::EOPNOTSUPP) | Some(libc::EINVAL)
        )
    }

    #[test]
    fn heap_exact_roundtrip() {
        let ring = match NetworkRing::new(16) {
            Ok(ring) => Rc::new(ring),
            Err(err) => {
                eprintln!("heap_exact_roundtrip: ring unavailable: {err}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let Some((a, b)) = socketpair() else {
            return;
        };
        let payload = vec![0x5a; 128 * 1024];
        let mut send = Box::pin(handle.send_exact(a, payload.clone()));
        let mut recv = Box::pin(handle.recv_exact(b, payload.len()));
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut sent = None;
        let mut received = None;

        for _ in 0..5_000_000 {
            if sent.is_none() {
                if let Poll::Ready(result) = send.as_mut().poll(&mut cx) {
                    sent = Some(result);
                }
            }
            if received.is_none() {
                if let Poll::Ready(result) = recv.as_mut().poll(&mut cx) {
                    received = Some(result);
                }
            }
            if sent.is_some() && received.is_some() {
                break;
            }
        }

        match sent.expect("exact send did not complete") {
            Ok(()) => {}
            Err(err) if is_unsupported(&err) => {
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Err(err) => panic!("send_exact failed: {err}"),
        }
        assert_eq!(received.unwrap().unwrap(), payload);
        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    #[test]
    fn recv_exact_reports_partial_eof() {
        let ring = match NetworkRing::new(8) {
            Ok(ring) => Rc::new(ring),
            Err(err) => {
                eprintln!("recv_exact_eof: ring unavailable: {err}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let Some((a, b)) = socketpair() else {
            return;
        };
        let part = b"short";
        assert_eq!(
            unsafe { libc::write(a, part.as_ptr().cast(), part.len()) },
            part.len() as isize
        );
        assert_eq!(unsafe { libc::shutdown(a, libc::SHUT_WR) }, 0);

        let mut recv = Box::pin(handle.recv_exact(b, part.len() + 1));
        let err = match block_on(recv.as_mut()) {
            Err(err) if is_unsupported(&err) => {
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Err(err) => err,
            Ok(_) => panic!("partial EOF unexpectedly succeeded"),
        };
        assert_eq!(err.kind(), io::ErrorKind::UnexpectedEof);
        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    #[test]
    fn recv_fixed_exact_joins_short_receives() {
        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE];
        let ring = match NetworkRing::new(8) {
            Ok(ring) => ring,
            Err(err) => {
                eprintln!("recv_fixed_exact: ring unavailable: {err}; skipping");
                return;
            }
        };
        if let Err(err) = ring.register_region(store.as_mut_ptr(), store.len()) {
            eprintln!("recv_fixed_exact: register failed: {err}; skipping");
            return;
        }
        let handle = NetHandle::new(Rc::new(ring));
        let Some((a, b)) = socketpair() else {
            return;
        };
        let first = b"frame ";
        let second = b"payload";
        assert_eq!(
            unsafe { libc::write(a, first.as_ptr().cast(), first.len()) },
            first.len() as isize
        );

        let mut recv = Box::pin(handle.recv_fixed_exact(b, 0, 31, first.len() + second.len()));
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(recv.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            unsafe { libc::write(a, second.as_ptr().cast(), second.len()) },
            second.len() as isize
        );
        match block_on(recv.as_mut()) {
            Ok(()) => assert_eq!(
                &store[31..31 + first.len() + second.len()],
                b"frame payload"
            ),
            Err(err) if is_unsupported(&err) => {}
            Err(err) => panic!("recv_fixed_exact failed: {err}"),
        }
        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    #[test]
    fn recv_fixed_exact_reports_partial_eof() {
        let mut store = vec![0u8; 64];
        let ring = match NetworkRing::new(8) {
            Ok(ring) => ring,
            Err(err) => {
                eprintln!("recv_fixed_exact_eof: ring unavailable: {err}; skipping");
                return;
            }
        };
        if let Err(err) = ring.register_region(store.as_mut_ptr(), store.len()) {
            eprintln!("recv_fixed_exact_eof: register failed: {err}; skipping");
            return;
        }
        let handle = NetHandle::new(Rc::new(ring));
        let Some((a, b)) = socketpair() else {
            return;
        };
        let part = b"short";
        assert_eq!(
            unsafe { libc::write(a, part.as_ptr().cast(), part.len()) },
            part.len() as isize
        );
        assert_eq!(unsafe { libc::shutdown(a, libc::SHUT_WR) }, 0);

        let mut recv = Box::pin(handle.recv_fixed_exact(b, 0, 7, part.len() + 1));
        match block_on(recv.as_mut()) {
            Err(err) if is_unsupported(&err) => {}
            Err(err) => assert_eq!(err.kind(), io::ErrorKind::UnexpectedEof),
            Ok(()) => panic!("partial fixed EOF unexpectedly succeeded"),
        }
        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    #[test]
    fn fixed_exact_send_zc_receive_roundtrip() {
        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE * 2];
        let payload = b"exact SEND_ZC frame";
        store[..payload.len()].copy_from_slice(payload);
        let ring = match NetworkRing::new(8) {
            Ok(ring) => ring,
            Err(err) => {
                eprintln!("fixed_exact_roundtrip: ring unavailable: {err}; skipping");
                return;
            }
        };
        if let Err(err) = ring.register_region(store.as_mut_ptr(), store.len()) {
            eprintln!("fixed_exact_roundtrip: register failed: {err}; skipping");
            return;
        }
        let handle = NetHandle::new(Rc::new(ring));
        let Some((a, b)) = tcp_loopback_pair() else {
            return;
        };
        let mut send = Box::pin(handle.send_zc_fixed_exact(a, 0, 0, payload.len()));
        let mut recv = Box::pin(handle.recv_fixed_exact(b, 0, PAGE, payload.len()));
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut sent = None;
        let mut received = None;

        for _ in 0..5_000_000 {
            if sent.is_none() {
                if let Poll::Ready(result) = send.as_mut().poll(&mut cx) {
                    sent = Some(result);
                }
            }
            if received.is_none() {
                if let Poll::Ready(result) = recv.as_mut().poll(&mut cx) {
                    received = Some(result);
                }
            }
            if sent.is_some() && received.is_some() {
                break;
            }
        }

        match sent.expect("exact SEND_ZC did not complete") {
            Ok(()) => {}
            Err(err) if is_unsupported(&err) => {
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Err(err) => panic!("send_zc_fixed_exact failed: {err}"),
        }
        received.unwrap().unwrap();
        assert_eq!(&store[PAGE..PAGE + payload.len()], payload);
        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }
}
