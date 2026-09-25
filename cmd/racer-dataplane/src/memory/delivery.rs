//! Independent reader cursors with owned sockets and bounded stall deadlines.
//!
//! Delivery copies immutable bytes into a bounded pipe, then splices to a socket.
//! Unsupported sockets fall back to nonblocking send from the last accepted byte.
//! Neither path lends userspace page pointers to the socket after return. Only
//! raw-socket readiness is asynchronous. On HTTP backpressure, an accounted send
//! transfers the entire connection/reader lease to the reactor, retaining admission
//! through abandonment and the final completion fence. Subsequent chunks splice.
use super::{
    pipe::{PipeLease, PipePool},
    pool::VerifiedPage,
};
use crate::{
    error::{Error, Operation, Result},
    http::{io::OwnedBuffer, pool::ConnectionLease},
    model::range::PageSlice,
    runtime::{deadline::RequestScope, reactor::IoBuffer},
};
use std::{
    future::poll_fn,
    io,
    os::fd::{AsFd, AsRawFd, OwnedFd},
    rc::Rc,
    task::Poll,
    time::{Duration, Instant},
};

// Limit both syscall size and work in one executor turn, even for a writable peer.
const SEND_CHUNK_BYTES: usize = 64 * 1024;
const SEND_BUDGET_BYTES: usize = 256 * 1024;
const SEND_BUDGET_CALLS: usize = 32;

pub struct Delivery {
    pipes: Rc<PipePool>,
    stall_timeout: Duration,
}

/// A reader pins an immutable page and a separately admitted pipe for its lifetime.
/// Each reader has its own staging pipe and socket-accepted cursor.
pub struct ReaderLease {
    page: VerifiedPage,
    pipe: PipeLease,
    slice: PageSlice,
    sent: usize,
    connection: Option<Rc<OwnedFd>>,
}

impl ReaderLease {
    pub fn slice(&self) -> PageSlice {
        self.slice
    }

    pub fn bytes_sent(&self) -> usize {
        self.sent
    }

    pub fn remaining(&self) -> usize {
        self.slice.length as usize - self.sent
    }

    /// Bind an exclusively owned socket for the legacy finish API. The socket is
    /// closed on completion; use finish_to to recover an HTTP connection lease.
    pub fn attach_connection(&mut self, connection: OwnedFd) -> Result<()> {
        if self.connection.is_some() {
            return Err(Error::InvalidRequest);
        }
        validate_socket(&connection)?;
        self.connection = Some(Rc::new(connection));
        Ok(())
    }

    fn try_send(&mut self, connection: &OwnedFd, copying: bool) -> io::Result<usize> {
        let start = self.slice.offset as usize + self.sent;
        let count = self.remaining().min(SEND_CHUNK_BYTES);
        let bytes = &self.page.bytes()[start..start + count];
        if copying {
            // SAFETY: send copies the live slice synchronously. DONTWAIT also
            // works for blocking descriptors; NOSIGNAL makes disconnect an error.
            let sent = unsafe {
                libc::send(
                    connection.as_raw_fd(),
                    bytes.as_ptr().cast(),
                    bytes.len(),
                    libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
                )
            };
            return if sent < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(sent as usize)
            };
        }
        // Refill only an empty pipe: a partial splice leaves the exact suffix.
        if self.pipe.buffered() == 0 && self.pipe.try_write(bytes)? == 0 {
            return Err(io::ErrorKind::WriteZero.into());
        }
        self.pipe.try_splice_to(connection, count)
    }
}

impl Delivery {
    pub fn new(pipes: Rc<PipePool>, stall_timeout: Duration) -> Self {
        Self {
            pipes,
            stall_timeout,
        }
    }

    pub fn attach(&self, page: VerifiedPage, slice: PageSlice) -> Result<ReaderLease> {
        let end = (slice.offset as usize)
            .checked_add(slice.length as usize)
            .ok_or(Error::InvalidRange)?;
        if slice.page != page.page().number || end > page.bytes().len() {
            return Err(Error::InvalidRange);
        }
        Ok(ReaderLease {
            page,
            pipe: self.pipes.acquire()?,
            slice,
            sent: 0,
            connection: None,
        })
    }

    pub fn attach_connection(
        &self,
        page: VerifiedPage,
        slice: PageSlice,
        connection: OwnedFd,
    ) -> Result<ReaderLease> {
        let mut reader = self.attach(page, slice)?;
        reader.attach_connection(connection)?;
        Ok(reader)
    }

    /// An unbound reader fails with InvalidRequest, even for an empty slice.
    pub fn finish<'a>(
        &'a self,
        mut reader: ReaderLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let connection = reader.connection.take().ok_or(Error::InvalidRequest)?;
            self.send_reader(reader, connection, scope).await
        })
    }

    /// Own the complete HTTP connection until the slice is sent, then return it
    /// for the next slice. Require a previously sent head with a known body length
    /// and update HTTP framing. Errors or abandonment cannot return it to a pool.
    pub fn finish_to<'a>(
        &'a self,
        mut reader: ReaderLease,
        mut connection: ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        // Also protect abandonment before the returned future's first poll.
        connection.begin_io();
        Box::pin(async move {
            scope.check()?;
            if reader.connection.is_some() {
                return Err(Error::InvalidRequest);
            }
            let remaining = connection.tx_remaining.ok_or(Error::InvalidRequest)?;
            let length = reader.remaining() as u64;
            if length > remaining {
                return Err(Error::InvalidRequest);
            }
            let socket = connection.socket();
            validate_socket(&*socket)?;
            drop(socket);
            let mut stalled_at = Instant::now();
            let mut copying = false;
            let mut budget = 0;
            let mut calls = 0;
            while reader.remaining() != 0 {
                scope.check()?;
                let mut send_scope = scope.clone();
                send_scope.deadline.0 = send_scope.deadline.0.min(
                    stalled_at
                        .checked_add(self.stall_timeout)
                        .ok_or(Error::InvalidConfiguration)?,
                );
                send_scope.check()?;
                let sent = match reader.try_send(&connection.socket(), copying) {
                    Ok(sent) => sent,
                    Err(error) if !copying && splice_unsupported(&error) => {
                        copying = true;
                        continue;
                    }
                    Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                        yield_once().await;
                        continue;
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        // Readiness currently retains only an FD, not connection
                        // admission. Instead submit one owned send on backpressure.
                        // Drain copied pipe bytes into its accounted buffer before
                        // submission. A partial send's unsent suffix is reconstructed
                        // from the immutable page on the next iteration.
                        let mut buffer = if copying {
                            let start = reader.slice.offset as usize + reader.sent;
                            let count = reader.remaining().min(SEND_CHUNK_BYTES);
                            OwnedBuffer::copy_from(
                                self.pipes.admission(),
                                &reader.page.bytes()[start..start + count],
                            )?
                        } else {
                            OwnedBuffer::new(self.pipes.admission(), reader.pipe.buffered())?
                        };
                        let count = buffer.bytes()?.len();
                        if !copying {
                            match reader.pipe.try_read(buffer.bytes_mut()?) {
                                Ok(read) if read == count && count != 0 => {}
                                Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                                    yield_once().await;
                                    continue;
                                }
                                _ => return Err(Error::Io),
                            }
                        }
                        let completion = self
                            .pipes
                            .reactor()
                            .send(
                                connection.socket(),
                                buffer,
                                (reader, connection),
                                &send_scope,
                            )
                            .await?;
                        (reader, connection) = completion.lease;
                        if completion.bytes > count {
                            return Err(Error::Io);
                        }
                        completion.bytes
                    }
                    Err(_) => return Err(Error::Io),
                };
                if sent == 0 || sent > reader.remaining() {
                    return Err(Error::Io);
                }
                reader.sent += sent;
                stalled_at = Instant::now();
                budget += sent;
                calls += 1;
                if (budget >= SEND_BUDGET_BYTES || calls >= SEND_BUDGET_CALLS)
                    && reader.remaining() != 0
                {
                    yield_once().await;
                    budget = 0;
                    calls = 0;
                }
            }
            connection.tx_remaining = Some(remaining - length);
            Ok(connection)
        })
    }

    /// Own an unframed client socket until the slice is sent, then return it.
    /// Errors or abandonment drop that owner. HTTP callers must use finish_to.
    ///
    /// Readiness retains the descriptor, so abandoning a future cannot recycle a
    /// descriptor still referenced by the reactor. Blocking input sockets use the
    /// MSG_DONTWAIT copy fallback; nonblocking sockets use the copied splice path.
    pub fn finish_to_socket<'a>(
        &'a self,
        reader: ReaderLease,
        connection: OwnedFd,
        scope: &'a RequestScope,
    ) -> Operation<'a, OwnedFd> {
        Box::pin(async move {
            scope.check()?;
            if reader.connection.is_some() {
                return Err(Error::InvalidRequest);
            }
            validate_socket(&connection)?;
            let connection = Rc::new(connection);
            self.send_reader(reader, connection.clone(), scope).await?;
            // Successful readiness must release its descriptor reference at its
            // completion fence. An unfenced reference cannot be returned as an
            // exclusively owned socket.
            Rc::try_unwrap(connection).map_err(|_| Error::Io)
        })
    }

    async fn send_reader(
        &self,
        mut reader: ReaderLease,
        connection: Rc<OwnedFd>,
        scope: &RequestScope,
    ) -> Result<()> {
        scope.check()?;
        let mut stalled_at = Instant::now();
        let mut budget = 0;
        let mut calls = 0;
        let mut copying = false;
        while reader.remaining() != 0 {
            scope.check()?;
            let stall_deadline = stalled_at
                .checked_add(self.stall_timeout)
                .ok_or(Error::InvalidConfiguration)?;
            if Instant::now() >= stall_deadline {
                return Err(Error::DeadlineExceeded);
            }
            let result = reader.try_send(&connection, copying);
            if let Ok(sent) = result {
                if sent == 0 || sent > reader.remaining() {
                    return Err(Error::Io);
                }
                reader.sent += sent;
                budget += sent;
                calls += 1;
                stalled_at = Instant::now();
                if (budget >= SEND_BUDGET_BYTES || calls >= SEND_BUDGET_CALLS)
                    && reader.remaining() != 0
                {
                    yield_once().await;
                    budget = 0;
                    calls = 0;
                }
                continue;
            }
            let error = result.unwrap_err();
            if !copying && splice_unsupported(&error) {
                // Any queued suffix is now ignored and closed with the reader.
                // The immutable page plus accepted cursor reconstructs it exactly
                // without allocating unaccounted fallback staging memory.
                copying = true;
                continue;
            }
            match error.kind() {
                io::ErrorKind::Interrupted => yield_once().await,
                io::ErrorKind::WouldBlock => {
                    let mut wait_scope = scope.clone();
                    wait_scope.deadline.0 = wait_scope.deadline.0.min(stall_deadline);
                    self.pipes
                        .reactor()
                        .readiness(connection.clone(), libc::POLLOUT as u32, &wait_scope)
                        .await?;
                    // Readiness may be spurious or report HUP. Retry send to get
                    // the real result, but never spin within one executor turn.
                    yield_once().await;
                }
                _ => return Err(Error::Io),
            }
        }
        Ok(())
    }
}

fn splice_unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}

fn validate_socket(connection: &impl AsFd) -> Result<()> {
    let mut kind: libc::c_int = 0;
    let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
    // SAFETY: both output pointers reference correctly sized writable values.
    let result = unsafe {
        libc::getsockopt(
            connection.as_fd().as_raw_fd(),
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

async fn yield_once() {
    let mut yielded = false;
    poll_fn(|cx| {
        if yielded {
            Poll::Ready(())
        } else {
            yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        memory::{pipe::tests::admission, pool::VerifiedBytes},
        model::{
            identity::{
                CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId,
                StrongEtag,
            },
            limits::ResourceClass,
        },
        runtime::{
            admission::Admission,
            deadline::{Cancellation, Deadline},
            reactor::Reactor,
        },
    };
    use std::{io::Read, os::unix::net::UnixStream, sync::Arc, task::Context};

    fn setup(pipes: usize, stall: Duration) -> (Rc<Admission>, Rc<Reactor>, Delivery) {
        let admission = admission(pipes);
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = Rc::new(PipePool::new(admission.clone(), reactor.clone()));
        (admission, reactor, Delivery::new(pool, stall))
    }

    fn scope() -> RequestScope {
        RequestScope {
            request: RequestId([0; 16]),
            deadline: Deadline(Instant::now() + Duration::from_secs(5)),
            cancellation: Cancellation::new().unwrap(),
        }
    }

    fn page(admission: &Admission, bytes: Vec<u8>) -> VerifiedPage {
        VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId("delivery-test".into()),
                            key: CacheKey([7; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    number: PageNumber(0),
                },
                reservation: admission
                    .reserve(None, ResourceClass::Plaintext, bytes.len().max(1))
                    .unwrap(),
                bytes: bytes.into(),
            }),
        }
    }

    fn slice(offset: u32, length: u32) -> PageSlice {
        PageSlice {
            page: PageNumber(0),
            offset,
            length,
        }
    }

    fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
        let start = Instant::now();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(
                start.elapsed() < Duration::from_secs(5),
                "delivery did not complete"
            );
            reactor.poll_budgeted(64).unwrap();
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    fn small_send_buffer(socket: &UnixStream) {
        socket.set_nonblocking(true).unwrap();
        let size: libc::c_int = 4096;
        // SAFETY: the option pointer refers to a live, correctly sized integer.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    socket.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&size as *const libc::c_int).cast(),
                    std::mem::size_of_val(&size) as libc::socklen_t,
                )
            },
            0
        );
    }

    #[test]
    fn independent_slices_share_page_until_last_reader_and_return_socket() {
        let (admission, reactor, delivery) = setup(2, Duration::from_secs(1));
        let page = page(&admission, b"0123456789".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let first = delivery.attach(page.clone(), slice(1, 3)).unwrap();
        let second = delivery.attach(page.clone(), slice(6, 4)).unwrap();
        assert_eq!(first.bytes_sent(), 0);
        assert_eq!(first.remaining(), 3);
        assert_eq!(second.slice(), slice(6, 4));
        drop(page);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let scope = scope();
        let socket = drive(
            &reactor,
            delivery.finish_to_socket(first, socket.into(), &scope),
        )
        .unwrap();
        assert!(weak.upgrade().is_some());
        let socket = drive(&reactor, delivery.finish_to_socket(second, socket, &scope)).unwrap();
        assert!(weak.upgrade().is_none());
        let mut bytes = [0; 7];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(&bytes, b"1236789");
        drop(socket);
        assert!(delivery.pipes.acquire().is_ok());
    }

    #[test]
    fn fallback_after_partial_splice_restarts_at_accepted_cursor() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"0123456789".to_vec());
        let mut reader = delivery.attach(page, slice(1, 8)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        reader.pipe.try_write(b"12345678").unwrap();
        reader.sent = reader.pipe.try_splice_to(&socket, 3).unwrap();
        assert_eq!(reader.sent, 3);
        assert_eq!(reader.pipe.buffered(), 5);
        // A blocking FD is deliberately unsupported by the splice API, but the
        // send fallback still cannot block and must not resend the accepted prefix.
        socket.set_nonblocking(false).unwrap();
        let socket = drive(
            &reactor,
            delivery.finish_to_socket(reader, socket.into(), &scope()),
        )
        .unwrap();
        drop(socket);
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"12345678");
    }

    #[test]
    fn tcp_splice_delivers_selected_slice_and_closes_on_legacy_finish() {
        use std::net::{TcpListener, TcpStream};
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        socket.set_nonblocking(true).unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
        let page = page(&admission, b"prefix-selected-suffix".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery
            .attach_connection(page, slice(7, 8), socket.into())
            .unwrap();
        drive(&reactor, delivery.finish(reader, &scope())).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"selected");
    }

    #[test]
    fn invalid_ranges_and_unbound_finish_fail_without_sending() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        for invalid in [
            slice(4, 0),
            slice(2, 2),
            slice(u32::MAX, u32::MAX),
            PageSlice {
                page: PageNumber(1),
                offset: 0,
                length: 1,
            },
        ] {
            assert!(matches!(
                delivery.attach(page.clone(), invalid),
                Err(Error::InvalidRange)
            ));
        }
        let scope = scope();
        let reader = delivery.attach(page.clone(), slice(0, 3)).unwrap();
        assert_eq!(
            drive(&reactor, delivery.finish(reader, &scope)),
            Err(Error::InvalidRequest)
        );
        let reader = delivery.attach(page, slice(3, 0)).unwrap();
        assert_eq!(
            drive(&reactor, delivery.finish(reader, &scope)),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn bound_legacy_finish_and_empty_slices_work() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let reader = delivery
            .attach_connection(page.clone(), slice(1, 2), socket.into())
            .unwrap();
        drive(&reactor, delivery.finish(reader, &scope())).unwrap();
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"bc");
        let (socket, _peer) = UnixStream::pair().unwrap();
        let reader = delivery.attach(page, slice(3, 0)).unwrap();
        assert!(
            drive(
                &reactor,
                delivery.finish_to_socket(reader, socket.into(), &scope())
            )
            .is_ok()
        );
    }

    #[test]
    fn cancelled_expired_and_disconnected_readers_release_resources() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        for failure in [Error::Cancelled, Error::DeadlineExceeded, Error::Io] {
            let mut scope = scope();
            let (socket, peer) = UnixStream::pair().unwrap();
            let page = page(&admission, b"abc".to_vec());
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 3)).unwrap();
            match failure {
                Error::Cancelled => scope.cancel().unwrap(),
                Error::DeadlineExceeded => scope.deadline = Deadline(Instant::now()),
                Error::Io => drop(peer),
                _ => unreachable!(),
            }
            assert!(
                matches!(drive(&reactor, delivery.finish_to_socket(reader, socket.into(), &scope)),
                Err(error) if error == failure)
            );
            assert!(weak.upgrade().is_none());
            assert!(delivery.pipes.acquire().is_ok());
        }
    }

    #[test]
    fn rejects_nonsockets_datagrams_and_duplicate_binding() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let reader = delivery.attach(page.clone(), slice(0, 3)).unwrap();
        let file = std::fs::File::open("/dev/null").unwrap();
        assert!(matches!(
            drive(
                &reactor,
                delivery.finish_to_socket(reader, file.into(), &scope())
            ),
            Err(Error::InvalidRequest)
        ));
        let mut reader = delivery.attach(page, slice(0, 3)).unwrap();
        let (datagram, _peer) = std::os::unix::net::UnixDatagram::pair().unwrap();
        assert_eq!(
            reader.attach_connection(datagram.into()),
            Err(Error::InvalidRequest)
        );
        let (socket, _peer) = UnixStream::pair().unwrap();
        reader.attach_connection(socket.into()).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        assert_eq!(
            reader.attach_connection(socket.into()),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn partial_sends_wait_and_deliver_exactly_once_without_blocking() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
        let bytes: Vec<u8> = (0..512 * 1024).map(|i| (i % 251) as u8).collect();
        let page = page(&admission, bytes.clone());
        let reader = delivery.attach(page, slice(0, bytes.len() as u32)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        peer.set_nonblocking(true).unwrap();
        let scope = scope();
        let mut operation = delivery.finish_to_socket(reader, socket.into(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        let mut received = Vec::new();
        let mut scratch = [0; 8192];
        let start = Instant::now();
        let mut complete = false;
        while received.len() != bytes.len() || !complete {
            assert!(start.elapsed() < Duration::from_secs(5));
            loop {
                match peer.read(&mut scratch) {
                    Ok(0) => break,
                    Ok(count) => received.extend_from_slice(&scratch[..count]),
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                    Err(error) => panic!("{error}"),
                }
            }
            reactor.poll_budgeted(64).unwrap();
            if !complete {
                if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                    result.unwrap();
                    complete = true;
                }
            }
        }
        assert_eq!(received, bytes);
    }

    #[test]
    fn stalled_reader_times_out_without_cancelling_another_reader() {
        let (admission, reactor, delivery) = setup(2, Duration::from_millis(25));
        let page = page(&admission, vec![0x5a; 512 * 1024]);
        let weak = Arc::downgrade(&page.inner);
        let slow = delivery.attach(page.clone(), slice(0, 512 * 1024)).unwrap();
        let fast = delivery.attach(page, slice(5, 3)).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        let scope = scope();
        assert!(matches!(
            drive(
                &reactor,
                delivery.finish_to_socket(slow, socket.into(), &scope)
            ),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(scope.check(), Ok(()));
        assert!(weak.upgrade().is_some());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        drive(
            &reactor,
            delivery.finish_to_socket(fast, socket.into(), &scope),
        )
        .unwrap();
        let mut bytes = [0; 3];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(bytes, [0x5a; 3]);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn abandonment_and_cancellation_of_pending_send_release_copied_page() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        for cancel in [false, true] {
            let page = page(&admission, vec![0x5a; 512 * 1024]);
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
            let (socket, _peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            let scope = scope();
            let mut operation = delivery.finish_to_socket(reader, socket.into(), &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert!(weak.upgrade().is_some());
            assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
            if cancel {
                scope.cancel().unwrap();
                assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
            } else {
                drop(operation);
            }
            assert!(weak.upgrade().is_none());
            assert!(delivery.pipes.acquire().is_ok());
            // Fence abandoned readiness registrations; they do not own page bytes.
            for _ in 0..4 {
                reactor.poll_budgeted(64).unwrap();
            }
        }
    }

    #[test]
    fn http_delivery_preserves_framing_and_owned_connection() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        // Equivalent to successful head I/O: there are no request body bytes and
        // the response head promised exactly three bytes.
        connection.rx_remaining = Some(0);
        connection.tx_remaining = Some(3);
        let reader = delivery.attach(page.clone(), slice(0, 2)).unwrap();
        let scope = scope();
        // A writable socket completes directly through the pipe without submitting
        // a staging send. Stopping the reactor makes an accidental submission fail.
        drive(&reactor, reactor.drain()).unwrap();
        let connection = drive(&reactor, delivery.finish_to(reader, connection, &scope)).unwrap();
        assert_eq!(connection.tx_remaining, Some(1));
        assert!(!connection.is_reusable());
        let reader = delivery.attach(page, slice(2, 1)).unwrap();
        let mut connection =
            drive(&reactor, delivery.finish_to(reader, connection, &scope)).unwrap();
        assert_eq!(connection.tx_remaining, Some(0));
        connection.finish_exchange().unwrap();
        assert!(connection.is_reusable());
        let mut bytes = [0; 3];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(&bytes, b"abc");
    }

    #[test]
    fn http_delivery_rejects_missing_or_insufficient_body_framing() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        for remaining in [None, Some(2)] {
            let page = page(&admission, b"abc".to_vec());
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            connection.tx_remaining = remaining;
            let reader = delivery.attach(page, slice(0, 3)).unwrap();
            assert!(matches!(
                drive(&reactor, delivery.finish_to(reader, connection, &scope())),
                Err(Error::InvalidRequest)
            ));
            let mut bytes = [0; 1];
            assert_eq!(peer.read(&mut bytes).unwrap(), 0);
        }
    }

    #[test]
    fn unpolled_future_releases_page_pipe_and_owned_connection() {
        let (admission, _reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 3)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let scope = scope();
        let operation = delivery.finish_to_socket(reader, socket.into(), &scope);
        assert!(weak.upgrade().is_some());
        assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
        drop(operation);
        assert!(weak.upgrade().is_none());
        assert!(delivery.pipes.acquire().is_ok());
        let mut bytes = [0; 1];
        assert_eq!(peer.read(&mut bytes).unwrap(), 0);
    }

    #[test]
    fn http_connection_and_reader_admission_survive_readiness_wait() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, vec![0x5a; 512 * 1024]);
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        connection.tx_remaining = Some(512 * 1024);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert!(reactor.in_flight() > 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        scope.cancel().unwrap();
        assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn original_deadline_caps_a_longer_stall_timeout() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(30));
        let reader = delivery
            .attach(
                page(&admission, vec![0x5a; 512 * 1024]),
                slice(0, 512 * 1024),
            )
            .unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        let mut scope = scope();
        scope.deadline = Deadline(Instant::now() + Duration::from_millis(25));
        let original = scope.deadline.0;
        assert!(matches!(
            drive(
                &reactor,
                delivery.finish_to_socket(reader, socket.into(), &scope)
            ),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(scope.deadline.0, original);
    }

    #[test]
    fn abandoned_http_send_does_not_cycle_through_reactor_owner() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let weak_reactor = Rc::downgrade(&reactor);
        let page = page(&admission, vec![0x5a; 512 * 1024]);
        let weak_page = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        connection.tx_remaining = Some(512 * 1024);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert!(reactor.in_flight() > 0);
        drop(operation);
        drop(delivery);
        // The pending lease must not own an Rc back to its containing reactor.
        assert_eq!(Rc::strong_count(&reactor), 1);
        drop(reactor);
        assert!(weak_reactor.upgrade().is_none());
        assert!(weak_page.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn abandoned_http_send_retains_all_leases_until_completion_fence() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, vec![0x5a; 512 * 1024]);
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        connection.tx_remaining = Some(512 * 1024);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        drop(operation);
        assert!(weak.upgrade().is_some());
        assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        let start = Instant::now();
        while reactor.in_flight() != 0 {
            assert!(start.elapsed() < Duration::from_secs(5));
            reactor.poll_budgeted(64).unwrap();
            std::thread::sleep(Duration::from_millis(1));
        }
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert!(delivery.pipes.acquire().is_ok());
    }

    #[test]
    fn http_splice_and_backpressure_send_deliver_exactly_once() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
        let bytes: Vec<u8> = (0..512 * 1024).map(|i| (i % 251) as u8).collect();
        let page = page(&admission, bytes.clone());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, bytes.len() as u32)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        peer.set_nonblocking(true).unwrap();
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        connection.tx_remaining = Some(bytes.len() as u64);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        let mut received = Vec::new();
        let mut scratch = [0; 8192];
        let start = Instant::now();
        let mut completed = None;
        while received.len() != bytes.len() || completed.is_none() {
            assert!(start.elapsed() < Duration::from_secs(5));
            loop {
                match peer.read(&mut scratch) {
                    Ok(0) => break,
                    Ok(count) => received.extend_from_slice(&scratch[..count]),
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                    Err(error) => panic!("{error}"),
                }
            }
            reactor.poll_budgeted(64).unwrap();
            if completed.is_none() {
                if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                    completed = Some(result.unwrap());
                }
            }
        }
        assert_eq!(received, bytes);
        assert_eq!(completed.as_ref().unwrap().tx_remaining, Some(0));
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        drop(completed);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn pending_http_send_cancellation_disconnect_and_stall_release_all_leases() {
        let (admission, reactor, delivery) = setup(1, Duration::from_millis(25));
        for failure in [Error::Cancelled, Error::Io, Error::DeadlineExceeded] {
            let page = page(&admission, vec![0x5a; 512 * 1024]);
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
            let (socket, peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            connection.tx_remaining = Some(512 * 1024);
            let scope = scope();
            let original = scope.deadline.0;
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            match failure {
                Error::Cancelled => scope.cancel().unwrap(),
                Error::Io => drop(peer),
                _ => {}
            }
            assert!(matches!(drive(&reactor, operation), Err(error) if error == failure));
            assert_eq!(scope.deadline.0, original);
            if failure != Error::Cancelled {
                assert_eq!(scope.check(), Ok(()));
            }
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
}
