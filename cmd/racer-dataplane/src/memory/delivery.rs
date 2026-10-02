//! Independent reader cursors with owned sockets and bounded stall deadlines.
//!
//! Delivery copies immutable bytes into a bounded pipe, then splices to a socket.
//! Unsupported sockets fall back to nonblocking send from the last accepted byte.
//! Neither path lends userspace page pointers to the socket after return.
//! On HTTP backpressure, an accounted send
//! transfers the entire connection/reader lease to the reactor, retaining admission
//! through abandonment and the final completion fence. After backpressure, the
//! rest of that page uses direct sends instead of repeating pipe drain round trips.
//! Backpressured direct sends own immutable page views, not copied staging bytes.
use super::{
    pipe::{PipeLease, PipePool},
    pool::VerifiedPage,
};
use crate::runtime::reactor::Descriptor;
use crate::{
    error::{Error, Operation, Result},
    http::connection::{ConnectionLease, OwnedBuffer},
    model::PageSlice,
    runtime::{
        deadline::RequestScope,
        reactor::{IoBuffer, SendBuffer},
    },
};
#[cfg(test)]
use std::os::fd::AsRawFd;
#[cfg(test)]
use std::task::Poll;
#[cfg(test)]
use std::time::Instant;
use std::{io, rc::Rc, time::Duration};

// Limit both syscall size and work in one executor turn, even for a writable peer.
const SEND_CHUNK_BYTES: usize = 64 * 1024;
const SEND_BUDGET_BYTES: usize = 256 * 1024;
const SEND_BUDGET_CALLS: usize = 32;

/// One current final send, never an admission to release its CQE-owned leases.
#[derive(Default)]
pub(crate) struct FinalSend(std::cell::Cell<Option<bool>>);
impl FinalSend {
    pub(crate) fn provisional_release(&self) -> Result<()> {
        if self.0.get() != Some(false) {
            return Err(Error::InvalidRequest);
        }
        self.0.set(Some(true));
        Ok(())
    }
}

/// A fixed immutable view owns the full admitted page through the send fence.
/// It deliberately implements no receive or mutable-buffer capability.
struct PageSendRange {
    page: VerifiedPage,
    range: std::ops::Range<usize>,
}

impl PageSendRange {
    fn new(page: VerifiedPage, range: std::ops::Range<usize>) -> Result<Self> {
        if page.bytes().get(range.clone()).is_none() {
            return Err(Error::InvalidRange);
        }
        Ok(Self { page, range })
    }
}

// SAFETY: fixed immutable view retains the complete ciphertext page owner.
unsafe impl SendBuffer for PageSendRange {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(&self.page.bytes()[self.range.clone()])
    }
}

enum DeliveryBuffer {
    Pipe(OwnedBuffer),
    Page(PageSendRange),
}

// SAFETY: both variants retain stable immutable backing through completion.
unsafe impl SendBuffer for DeliveryBuffer {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        match self {
            Self::Pipe(buffer) => buffer.send_bytes(),
            Self::Page(buffer) => buffer.send_bytes(),
        }
    }
}

pub struct Delivery {
    metrics: crate::telemetry::metrics::Metrics,
    pipes: Rc<PipePool>,
    stall_timeout: Duration,
}

/// A reader pins an immutable page and a separately admitted pipe for its lifetime.
/// Each reader has its own staging pipe and socket-accepted cursor.
pub struct ReaderLease {
    _active: crate::telemetry::metrics::GaugeLease,
    page: VerifiedPage,
    pipe: PipeLease,
    slice: PageSlice,
    sent: usize,
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

    fn try_send(&mut self, connection: &Descriptor, copying: bool) -> io::Result<usize> {
        let start = self.slice.offset as usize + self.sent;
        let count = self.remaining().min(SEND_CHUNK_BYTES);
        let bytes = &self.page.bytes()[start..start + count];
        if copying {
            return connection.try_send(bytes);
        }
        // Refill only an empty pipe: a partial splice leaves the exact suffix.
        if self.pipe.buffered() == 0 && self.pipe.try_write(bytes)? == 0 {
            return Err(io::ErrorKind::WriteZero.into());
        }
        self.pipe.try_splice_descriptor(connection, count)
    }
}

impl Delivery {
    pub fn new(pipes: Rc<PipePool>, stall_timeout: Duration) -> Self {
        Self {
            metrics: crate::telemetry::metrics::Metrics::default(),
            pipes,
            stall_timeout,
        }
    }
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::metrics::Metrics) -> Self {
        self.metrics = metrics;
        self
    }

    pub fn attach(&self, page: VerifiedPage, slice: PageSlice) -> Result<ReaderLease> {
        validate_slice(&page, slice)?;
        self.attach_reserved(page, slice, self.pipes.acquire()?)
    }

    pub(crate) fn admit<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, PipeLease> {
        self.pipes.acquire_wait(scope)
    }

    pub(crate) fn attach_reserved(
        &self,
        page: VerifiedPage,
        slice: PageSlice,
        pipe: PipeLease,
    ) -> Result<ReaderLease> {
        validate_slice(&page, slice)?;
        Ok(ReaderLease {
            _active: self
                .metrics
                .lease(crate::telemetry::metrics::Gauge::ActiveDeliveries)?,
            page,
            pipe,
            slice,
            sent: 0,
        })
    }

    /// Own the complete HTTP connection until the slice is sent, then return it
    /// for the next slice. Require a previously sent head with a known body length
    /// and update HTTP framing. Errors or abandonment cannot return it to a pool.
    pub fn finish_to<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, false, None)
    }

    /// Client body writes are bounded by lack of socket progress, not total
    /// object duration. Peer writes retain their absolute operation deadline.
    #[cfg(test)]
    pub(crate) fn finish_progressing<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, true, None)
    }

    pub(crate) fn finish_subscription<'a>(
        &'a self,
        reader: ReaderLease,
        connection: ConnectionLease,
        scope: &'a RequestScope,
        final_send: &'a FinalSend,
    ) -> Operation<'a, ConnectionLease> {
        self.finish_to_inner(reader, connection, scope, true, Some(final_send))
    }

    fn finish_to_inner<'a>(
        &'a self,
        mut reader: ReaderLease,
        mut connection: ConnectionLease,
        scope: &'a RequestScope,
        progressing: bool,
        final_send: Option<&'a FinalSend>,
    ) -> Operation<'a, ConnectionLease> {
        // Also protect abandonment before the returned future's first poll.
        connection.begin_io();
        Box::pin(async move {
            if !progressing {
                scope.check()?;
            }
            let remaining = connection.send_remaining().ok_or(Error::InvalidRequest)?;
            let length = reader.remaining() as u64;
            if length > remaining {
                return Err(Error::InvalidRequest);
            }
            let socket = connection.socket();
            validate_socket(&socket)?;
            drop(socket);
            let mut stalled_at = crate::runtime::environment::now();
            let mut copying = false;
            let mut budget = 0;
            let mut calls = 0;
            while reader.remaining() != 0 {
                if !progressing {
                    scope.check()?;
                }
                let mut send_scope = scope.clone();
                let stall_deadline = stalled_at
                    .checked_add(self.stall_timeout)
                    .ok_or(Error::InvalidConfiguration)?;
                send_scope.deadline.0 = if progressing {
                    stall_deadline
                } else {
                    send_scope.deadline.0.min(stall_deadline)
                };
                send_scope.check()?;
                let sent = match reader.try_send(&connection.socket(), copying) {
                    Ok(sent) => {
                        if copying {
                            let _ = self.metrics.record(
                                crate::telemetry::metrics::Event::DeliveryDirectBytes,
                                sent as u64,
                            );
                        }
                        sent
                    }
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
                        // Drain the first copied pipe into accounted storage. Later
                        // sends own immutable page views, avoiding repeated staging
                        // allocation, copy, and wipe. The reactor also retains the
                        // entire reader/pipe/connection lease until its final fence.
                        let buffer = if copying {
                            let start = reader.slice.offset as usize + reader.sent;
                            let count = reader.remaining().min(SEND_CHUNK_BYTES);
                            DeliveryBuffer::Page(PageSendRange::new(
                                reader.page.clone(),
                                start..start + count,
                            )?)
                        } else {
                            let count = reader.pipe.buffered();
                            let mut buffer = OwnedBuffer::new(self.pipes.admission(), count)?;
                            match reader.pipe.try_read(buffer.bytes_mut()?) {
                                Ok(read) if read == count && count != 0 => {}
                                Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                                    yield_once().await;
                                    continue;
                                }
                                _ => return Err(Error::Io),
                            }
                            let _ = self
                                .metrics
                                .record(crate::telemetry::metrics::Event::DeliveryPipeDrain, 1);
                            DeliveryBuffer::Pipe(buffer)
                        };
                        let count = buffer.send_bytes()?.len();
                        if let Some(state) = final_send {
                            state.0.set((count == reader.remaining()).then_some(false));
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
                        if let Some(state) = final_send {
                            let released = state.0.replace(None) == Some(true);
                            if released && completion.bytes != count {
                                return Err(Error::InvalidRequest);
                            }
                        }
                        if completion.bytes > count {
                            return Err(Error::Io);
                        }
                        // The pipe is empty and the immutable page reconstructs
                        // any unsent suffix. Avoid another write/splice/drain on
                        // the next backpressured chunk; keep the owned-send fence.
                        copying = true;
                        let _ = self.metrics.record(
                            crate::telemetry::metrics::Event::DeliveryDirectBytes,
                            completion.bytes as u64,
                        );
                        completion.bytes
                    }
                    Err(_) => return Err(Error::Io),
                };
                if sent == 0 || sent > reader.remaining() {
                    return Err(Error::Io);
                }
                reader.sent += sent;
                stalled_at = crate::runtime::environment::now();
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
            connection.consume_sent(usize::try_from(length).map_err(|_| Error::InvalidRequest)?)?;
            Ok(connection)
        })
    }
}

fn validate_slice(page: &VerifiedPage, slice: PageSlice) -> Result<()> {
    let end = (slice.offset as usize)
        .checked_add(slice.length as usize)
        .ok_or(Error::InvalidRange)?;
    if slice.page != page.page().number || end > page.bytes().len() {
        return Err(Error::InvalidRange);
    }
    Ok(())
}

fn splice_unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}

fn validate_socket(connection: &Descriptor) -> Result<()> {
    connection.validate_socket().map_err(Into::into)
}

use crate::error::cooperative_turn as yield_once;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        memory::{pipe::tests::admission, pool::VerifiedBytes},
        model::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId,
            ResourceClass, StrongEtag,
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
            body_deadlines: None,
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

    fn connection(socket: Descriptor, admission: &Admission, length: u64) -> ConnectionLease {
        let mut connection = crate::http::connection::from_accepted(socket, admission).unwrap();
        connection.set_framing(None, Some(length), false);
        connection
    }

    fn blocked_reader(
        admission: &Admission,
        delivery: &Delivery,
    ) -> (
        ReaderLease,
        ConnectionLease,
        UnixStream,
        std::sync::Weak<VerifiedBytes>,
    ) {
        let page = page(admission, vec![0x5a; 512 * 1024]);
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 512 * 1024)).unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        (
            reader,
            connection(socket.into(), admission, 512 * 1024),
            peer,
            weak,
        )
    }
    fn assert_pending<T>(operation: &mut Operation<'_, T>) {
        assert!(
            operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
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

    fn thread_cpu() -> Duration {
        let mut time = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        // SAFETY: clock_gettime initializes the live timespec on success.
        assert_eq!(
            unsafe { libc::clock_gettime(libc::CLOCK_THREAD_CPUTIME_ID, &mut time) },
            0
        );
        Duration::new(time.tv_sec as u64, time.tv_nsec as u32)
    }

    #[test]
    fn page_send_range_is_a_stable_admitted_immutable_subrange() {
        let (admission, _, _) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"prefix-selected-suffix".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let pointer = page.bytes()[7..].as_ptr();
        for range in [8..7, 0..23, usize::MAX..usize::MAX] {
            assert!(matches!(
                PageSendRange::new(page.clone(), range),
                Err(Error::InvalidRange)
            ));
        }
        let empty = PageSendRange::new(page.clone(), 22..22).unwrap();
        assert!(empty.send_bytes().unwrap().is_empty());
        drop(empty);
        let view = PageSendRange::new(page, 7..15).unwrap();
        assert_eq!(view.send_bytes().unwrap(), b"selected");
        assert_eq!(view.send_bytes().unwrap().as_ptr(), pointer);
        assert_eq!(admission.used(ResourceClass::Plaintext), 22);
        let moved = Box::new(view);
        assert_eq!(moved.send_bytes().unwrap().as_ptr(), pointer);
        drop(moved);
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn copying_http_send_keeps_full_admission_until_failure_fence() {
        for failure in ["abandon", "cancel", "disconnect", "deadline", "drain"] {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
            let page = page(&admission, vec![0x5a; 512 * 1024]);
            let weak = Arc::downgrade(&page.inner);
            let reader = delivery.attach(page, slice(7, 500 * 1024)).unwrap();
            let (socket, mut peer) = UnixStream::pair().unwrap();
            small_send_buffer(&socket);
            peer.set_nonblocking(true).unwrap();
            let mut connection =
                crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
            connection.set_framing(None, Some(500 * 1024), false);
            let mut scope = scope();
            if failure == "deadline" {
                scope.deadline = Deadline(Instant::now() + Duration::from_millis(100));
            }
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(weak.strong_count(), 1, "first send owns a pipe drain");
            let start = Instant::now();
            while weak.strong_count() == 1 {
                assert!(start.elapsed() < Duration::from_secs(1));
                loop {
                    match peer.read(&mut [0; 8192]) {
                        Ok(0) => panic!("unexpected EOF"),
                        Ok(_) => {}
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                        Err(error) => panic!("{error}"),
                    }
                }
                reactor.poll_budgeted(64).unwrap();
                assert!(operation.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(weak.strong_count(), 2, "reader and immutable send view");
            assert_eq!(reactor.in_flight(), 1);
            assert_eq!(admission.used(ResourceClass::Plaintext), 512 * 1024);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
            match failure {
                "abandon" => {
                    drop(operation);
                    assert_eq!(weak.strong_count(), 2);
                    assert_eq!(admission.used(ResourceClass::Plaintext), 512 * 1024);
                    assert_eq!(admission.used(ResourceClass::Connection), 1);
                    assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
                    drive(&reactor, reactor.drain()).unwrap();
                }
                "drain" => {
                    drive(&reactor, reactor.drain()).unwrap();
                    assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
                }
                _ => {
                    let expected = match failure {
                        "cancel" => {
                            scope.cancel().unwrap();
                            Error::Cancelled
                        }
                        "disconnect" => {
                            drop(peer);
                            Error::Io
                        }
                        _ => Error::DeadlineExceeded,
                    };
                    assert!(matches!(drive(&reactor, operation), Err(error) if error == expected));
                }
            }
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert!(delivery.pipes.acquire().is_ok());
        }
    }

    /// Real TCP, bounded send queue, and receiver pacing force repeated owned sends.
    /// Time only sender future/reactor turns, excluding receiver reads, validation,
    /// and pacing. This is thread CPU, not total kernel/worker CPU or throughput.
    /// Run with --ignored --nocapture --test-threads=1 before/after production edits.
    #[test]
    #[ignore = "local TCP delivery CPU benchmark"]
    fn loopback_backpressure_thread_cpu() {
        use std::net::{TcpListener, TcpStream};
        const LENGTH: usize = 512 * 1024;
        const PAGES: usize = 1024;
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(5));
        let bytes: Vec<u8> = (0..LENGTH).map(|i| (i % 251) as u8).collect();
        let page = page(&admission, bytes.clone());
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        socket.set_nonblocking(true).unwrap();
        socket.set_nodelay(true).unwrap();
        peer.set_nonblocking(true).unwrap();
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
        let mut connection =
            crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        let mut scratch = [0; 64 * 1024];
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for sample in 0..4 {
            let wall = Instant::now();
            let mut cpu = Duration::ZERO;
            let mut pending_turns = 0;
            let drains = delivery
                .metrics
                .count(crate::telemetry::metrics::Event::DeliveryPipeDrain);
            for _ in 0..PAGES {
                let scope = scope();
                connection.set_framing(connection.receive_remaining(), Some(LENGTH as u64), false);
                let reader = delivery
                    .attach(page.clone(), slice(0, LENGTH as u32))
                    .unwrap();
                let mut operation = delivery.finish_to(reader, connection, &scope);
                let mut completed = None;
                let mut received = 0;
                while completed.is_none() || received != LENGTH {
                    assert!(wall.elapsed() < Duration::from_secs(60));
                    let start = thread_cpu();
                    reactor.poll_budgeted(64).unwrap();
                    if completed.is_none() {
                        if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                            completed = Some(result.unwrap());
                        } else if reactor.in_flight() != 0 {
                            pending_turns += 1;
                        }
                    }
                    cpu += thread_cpu() - start;
                    loop {
                        match peer.read(&mut scratch) {
                            Ok(0) => panic!("unexpected EOF"),
                            Ok(count) => {
                                assert_eq!(&scratch[..count], &bytes[received..received + count]);
                                received += count;
                                let quick_ack: libc::c_int = 1;
                                // SAFETY: live TCP socket and correctly sized option.
                                assert_eq!(
                                    unsafe {
                                        libc::setsockopt(
                                            peer.as_raw_fd(),
                                            libc::IPPROTO_TCP,
                                            libc::TCP_QUICKACK,
                                            (&quick_ack as *const libc::c_int).cast(),
                                            std::mem::size_of_val(&quick_ack) as libc::socklen_t,
                                        )
                                    },
                                    0
                                );
                            }
                            Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                            Err(error) => panic!("{error}"),
                        }
                    }
                    // Let TCP and completion processing progress without charging
                    // busy polling or receiver pacing to the sender CPU sample.
                    std::thread::sleep(Duration::from_micros(50));
                }
                connection = completed.unwrap();
                assert_eq!(connection.send_remaining(), Some(0));
                assert_eq!(reactor.in_flight(), 0);
            }
            let drains = delivery
                .metrics
                .count(crate::telemetry::metrics::Event::DeliveryPipeDrain)
                - drains;
            assert!(drains > 0 && pending_turns > PAGES);
            eprintln!(
                "delivery_tcp sample={sample} warmup={} bytes={} sender_cpu_ms={:.3} cpu_ns_per_byte={:.4} wall_ms={} pipe_drains={drains} pending_turns={pending_turns}",
                sample == 0,
                LENGTH * PAGES,
                cpu.as_secs_f64() * 1000.0,
                cpu.as_nanos() as f64 / (LENGTH * PAGES) as f64,
                wall.elapsed().as_millis()
            );
        }
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
            delivery.finish_to(first, connection(socket.into(), &admission, 7), &scope),
        )
        .unwrap();
        assert!(weak.upgrade().is_some());
        let socket = drive(&reactor, delivery.finish_to(second, socket, &scope)).unwrap();
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
            delivery.finish_to(reader, connection(socket.into(), &admission, 5), &scope()),
        )
        .unwrap();
        drop(socket);
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"12345678");
    }

    #[test]
    fn tcp_splice_delivers_selected_slice_and_returns_connection() {
        use std::net::{TcpListener, TcpStream};
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        socket.set_nonblocking(true).unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
        let page = page(&admission, b"prefix-selected-suffix".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(7, 8)).unwrap();
        let connection = connection(socket.into(), &admission, 8);
        drop(drive(&reactor, delivery.finish_to(reader, connection, &scope())).unwrap());
        assert!(weak.upgrade().is_none());
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            1,
            "idle pipe remains admitted"
        );
        assert!(delivery.pipes.acquire().is_ok());
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"selected");
    }

    #[test]
    fn invalid_ranges_and_missing_framing_fail_without_sending() {
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
        let (socket, _peer) = UnixStream::pair().unwrap();
        let unframed = crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(&reactor, delivery.finish_to(reader, unframed, &scope)),
            Err(Error::InvalidRequest)
        ));
        let reader = delivery.attach(page, slice(3, 0)).unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let unframed = crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(&reactor, delivery.finish_to(reader, unframed, &scope)),
            Err(Error::InvalidRequest)
        ));
    }

    #[test]
    fn framed_delivery_and_empty_slices_work() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let reader = delivery.attach(page.clone(), slice(1, 2)).unwrap();
        drop(
            drive(
                &reactor,
                delivery.finish_to(reader, connection(socket.into(), &admission, 2), &scope()),
            )
            .unwrap(),
        );
        let mut bytes = Vec::new();
        peer.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"bc");
        let (socket, _peer) = UnixStream::pair().unwrap();
        let reader = delivery.attach(page, slice(3, 0)).unwrap();
        assert!(
            drive(
                &reactor,
                delivery.finish_to(reader, connection(socket.into(), &admission, 0), &scope())
            )
            .is_ok()
        );
    }

    #[test]
    fn canceled_expired_and_disconnected_readers_release_resources() {
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
                matches!(drive(&reactor, delivery.finish_to(reader, connection(socket.into(), &admission, 3), &scope)),
                Err(error) if error == failure)
            );
            assert!(weak.upgrade().is_none());
            assert!(delivery.pipes.acquire().is_ok());
        }
    }

    #[test]
    fn rejects_nonsockets_and_datagrams() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let reader = delivery.attach(page.clone(), slice(0, 3)).unwrap();
        let file = std::fs::File::open("/dev/null").unwrap();
        assert!(matches!(
            drive(
                &reactor,
                delivery.finish_to(reader, connection(file.into(), &admission, 3), &scope())
            ),
            Err(Error::InvalidRequest)
        ));
        let reader = delivery.attach(page, slice(0, 3)).unwrap();
        let (datagram, _peer) = std::os::unix::net::UnixDatagram::pair().unwrap();
        assert!(matches!(
            drive(
                &reactor,
                delivery.finish_to(reader, connection(datagram.into(), &admission, 3), &scope())
            ),
            Err(Error::InvalidRequest)
        ));
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
        let mut operation = delivery.finish_to(
            reader,
            connection(socket.into(), &admission, bytes.len() as u64),
            &scope,
        );
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
    fn stalled_reader_times_out_without_canceling_another_reader() {
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
                delivery.finish_to(
                    slow,
                    connection(socket.into(), &admission, 512 * 1024),
                    &scope
                )
            ),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(scope.check(), Ok(()));
        assert!(weak.upgrade().is_some());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        drive(
            &reactor,
            delivery.finish_to(fast, connection(socket.into(), &admission, 3), &scope),
        )
        .unwrap();
        let mut bytes = [0; 3];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(bytes, [0x5a; 3]);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn abandonment_and_cancellation_of_pending_send_release_copied_page() {
        for cancel in [false, true] {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
            let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            assert_pending(&mut operation);
            assert!(weak.upgrade().is_some());
            assert!(matches!(delivery.pipes.acquire(), Err(Error::Overloaded)));
            if cancel {
                scope.cancel().unwrap();
                assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
            } else {
                drop(operation);
                assert!(weak.upgrade().is_some());
                drive(&reactor, reactor.drain()).unwrap();
            }
            assert!(weak.upgrade().is_none());
            assert!(delivery.pipes.acquire().is_ok());
            assert_eq!(reactor.in_flight(), 0);
        }
    }

    #[test]
    fn http_delivery_preserves_framing_and_owned_connection() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let mut connection =
            crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        // Equivalent to successful head I/O: there are no request body bytes and
        // the response head promised exactly three bytes.
        connection.set_framing(Some(0), Some(3), false);
        let reader = delivery.attach(page.clone(), slice(0, 2)).unwrap();
        let scope = scope();
        // A writable socket completes through the pipe before the reactor is driven.
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(connection)) = operation.as_mut().poll(&mut cx) else {
            panic!("writable HTTP socket did not complete synchronously");
        };
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(connection.send_remaining(), Some(1));
        assert!(!connection.is_reusable());
        let reader = delivery.attach(page, slice(2, 1)).unwrap();
        let mut connection =
            drive(&reactor, delivery.finish_to(reader, connection, &scope)).unwrap();
        assert_eq!(connection.send_remaining(), Some(0));
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
            let mut connection =
                crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
            connection.set_framing(None, remaining, false);
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
        let operation =
            delivery.finish_to(reader, connection(socket.into(), &admission, 3), &scope);
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
        let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        assert_pending(&mut operation);
        assert!(reactor.in_flight() > 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        scope.cancel().unwrap();
        assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            1,
            "idle pipe remains admitted"
        );
        assert!(delivery.pipes.acquire().is_ok());
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn original_deadline_caps_a_longer_stall_timeout() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(30));
        let (reader, connection, _peer, _) = blocked_reader(&admission, &delivery);
        let mut scope = scope();
        scope.deadline = Deadline(Instant::now() + Duration::from_millis(25));
        let original = scope.deadline.0;
        assert!(matches!(
            drive(&reactor, delivery.finish_to(reader, connection, &scope)),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(scope.deadline.0, original);
    }

    #[test]
    fn abandoned_http_send_does_not_cycle_through_reactor_owner() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let weak_reactor = Rc::downgrade(&reactor);
        let (reader, connection, _peer, weak_page) = blocked_reader(&admission, &delivery);
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
        let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
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
    fn reactor_drain_fences_pending_http_delivery_and_releases_all_leases() {
        for abandon in [false, true] {
            let (admission, reactor, delivery) = setup(1, Duration::from_secs(30));
            let (reader, connection, _peer, weak) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let mut operation = delivery.finish_to(reader, connection, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            let mut operation = if abandon {
                drop(operation);
                None
            } else {
                Some(operation)
            };
            assert!(weak.upgrade().is_some());
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            // drain is a completion fence, not a blocking shutdown call. The
            // driver must keep polling CQEs until the outstanding send is fenced.
            drive(&reactor, reactor.drain()).unwrap();
            if let Some(operation) = operation.take() {
                assert!(matches!(drive(&reactor, operation), Err(Error::Cancelled)));
            }
            drop(operation);
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            // The reactor's own ring allocation remains admitted until drop.
            drop(delivery);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
            drop(reactor);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(scope.check(), Ok(()));
        }
    }

    #[test]
    fn http_splice_and_backpressure_send_deliver_exactly_once() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(2));
        let bytes: Vec<u8> = (0..512 * 1024).map(|i| (i % 251) as u8).collect();
        let mut backing = b"prefix!".to_vec();
        backing.extend_from_slice(&bytes);
        backing.extend_from_slice(b"suffix!");
        let page = page(&admission, backing);
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(7, bytes.len() as u32)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        small_send_buffer(&socket);
        peer.set_nonblocking(true).unwrap();
        let mut connection =
            crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        connection.set_framing(None, Some(bytes.len() as u64), false);
        let scope = scope();
        let mut operation = delivery.finish_to(reader, connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        let mut received = Vec::new();
        let mut scratch = [0; 8192];
        let start = Instant::now();
        let mut completed = None;
        let mut page_sends = 0;
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
                if weak.strong_count() == 2 {
                    page_sends += 1;
                }
            }
        }
        assert!(
            page_sends > 1,
            "exercise repeated immutable page sends and short writes"
        );
        assert_eq!(received, bytes);
        assert_eq!(completed.as_ref().unwrap().send_remaining(), Some(0));
        assert_eq!(
            delivery
                .metrics
                .count(crate::telemetry::metrics::Event::DeliveryPipeDrain),
            1,
            "a backpressured page drains its staging pipe only once"
        );
        assert!(
            delivery
                .metrics
                .count(crate::telemetry::metrics::Event::DeliveryDirectBytes)
                > 0
        );
        assert!(weak.upgrade().is_none());
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            1,
            "idle pipe remains admitted"
        );
        assert!(delivery.pipes.acquire().is_ok());
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        drop(completed);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn pending_http_send_cancellation_disconnect_and_stall_release_all_leases() {
        let (admission, reactor, delivery) = setup(1, Duration::from_millis(25));
        for failure in [Error::Cancelled, Error::Io, Error::DeadlineExceeded] {
            let (reader, connection, peer, weak) = blocked_reader(&admission, &delivery);
            let scope = scope();
            let original = scope.deadline.0;
            let mut operation = delivery.finish_progressing(reader, connection, &scope);
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
            assert_eq!(
                admission.used(ResourceClass::Pipe),
                1,
                "idle pipe remains admitted"
            );
            assert!(delivery.pipes.acquire().is_ok());
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }

    #[test]
    fn unpolled_http_delivery_closes_connection_and_releases_admission() {
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let page = page(&admission, b"abc".to_vec());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, 3)).unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let mut connection =
            crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        connection.set_framing(None, Some(3), false);
        let scope = scope();
        let operation = delivery.finish_to(reader, connection, &scope);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        drop(operation);
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            1,
            "idle pipe remains admitted"
        );
        assert!(delivery.pipes.acquire().is_ok());
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(peer.read(&mut [0; 1]).unwrap(), 0);
    }

    #[test]
    fn tcp_http_delivery_survives_page_and_pipe_release_before_peer_reads() {
        use std::net::{TcpListener, TcpStream};
        let (admission, reactor, delivery) = setup(1, Duration::from_secs(1));
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let (socket, _) = listener.accept().unwrap();
        let bytes: Vec<u8> = (0..4096).map(|i| (i % 251) as u8).collect();
        let page = page(&admission, bytes.clone());
        let weak = Arc::downgrade(&page.inner);
        let reader = delivery.attach(page, slice(0, bytes.len() as u32)).unwrap();
        let mut connection =
            crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        connection.set_framing(None, Some(bytes.len() as u64), false);
        let connection = drive(&reactor, delivery.finish_to(reader, connection, &scope())).unwrap();
        assert_eq!(connection.send_remaining(), Some(0));
        assert!(weak.upgrade().is_none());
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            1,
            "idle pipe remains admitted"
        );
        let mut replacement = delivery.pipes.acquire().unwrap();
        replacement.try_write(&[0xff; 4096]).unwrap();
        let mut received = vec![0; bytes.len()];
        peer.read_exact(&mut received).unwrap();
        assert_eq!(received, bytes);
        drop(connection);
        assert_eq!(peer.read(&mut [0; 1]).unwrap(), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
