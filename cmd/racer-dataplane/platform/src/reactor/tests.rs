//! Adapter tests: admission, accounting, rejection reports, the retry policy
//! (against a scripted backend) and real io_uring behavior.

use super::scripted::Scripted;
use super::*;
use std::{
    task::{Context, Poll, Waker},
    time::Instant,
};
use uring::backend::{OpKind, PushHints};

/// A request scope: a cancellation token and an optional deadline.
#[derive(Clone)]
pub(crate) struct RequestScope {
    pub(crate) id: u64,
    token: CancelToken,
    deadline: Option<Instant>,
}

impl RequestScope {
    pub(crate) fn cancel(&self) {
        self.token.cancel();
    }
}

impl LibraryScope for RequestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        if self.token.is_canceled() {
            Err(Error::Canceled)
        } else if self
            .deadline
            .is_some_and(|deadline| crate::environment::now() >= deadline)
        {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }

    fn cancellation(&self) -> Option<&CancelToken> {
        Some(&self.token)
    }
}

pub(crate) fn scope() -> RequestScope {
    scope_id(0)
}

pub(crate) fn scope_id(id: u64) -> RequestScope {
    RequestScope {
        id,
        token: CancelToken::new(),
        deadline: None,
    }
}

pub(crate) type TestReactor = Reactor<RequestScope, ()>;

pub(crate) fn poll<T, E>(future: &mut Operation<'_, T, E>) -> Poll<Result<T, E>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(Waker::noop()))
}

/// Drive explicit completion turns until `future` resolves.
pub(crate) fn drive<T, E, S: Scope, B: Budget>(
    reactor: &Reactor<S, B>,
    mut future: Operation<'_, T, E>,
) -> Result<T, E> {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

/// A real reactor, or `None` if io_uring is unavailable. With
/// `RUNTIME_REQUIRE_IO_URING=1` an unavailable kernel fails the test.
pub(crate) fn kernel_reactor(capacity: usize) -> Option<TestReactor> {
    let reactor = Reactor::new(capacity, ());
    match reactor.init() {
        Ok(()) => Some(reactor),
        Err(error) => {
            assert!(
                std::env::var_os("RUNTIME_REQUIRE_IO_URING").is_none_or(|value| value != "1"),
                "io_uring required but unavailable: {error}"
            );
            None
        }
    }
}

fn scripted(
    capacity: usize,
    responder: impl FnMut(OpKind, scripted::Region) -> Option<i32> + 'static,
) -> (TestReactor, Rc<RefCell<scripted::State>>) {
    let backend = Scripted::new(responder);
    let state = backend.state();
    (
        Reactor::with_backend(capacity, (), RacerBackend::Scripted(backend)),
        state,
    )
}

/// Plain stable storage.
struct Bytes(Vec<u8>);

// SAFETY: the Vec is never resized, so its heap storage is stable across moves.
unsafe impl IoBuffer for Bytes {
    type Error = Error;

    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }

    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}

fn null_fd() -> Rc<Descriptor> {
    Rc::new(Descriptor::from(std::fs::File::open("/dev/null").unwrap()))
}

/// Records charges and their release.
#[derive(Clone, Default)]
struct Meter(Rc<Cell<usize>>, Rc<Cell<bool>>);

struct MeterCharge(Rc<Cell<usize>>, usize);

impl Drop for MeterCharge {
    fn drop(&mut self) {
        self.0.set(self.0.get() - self.1);
    }
}

impl Budget for Meter {
    type Charge = MeterCharge;

    fn charge(&self, bytes: usize) -> Result<MeterCharge> {
        if self.1.get() {
            return Err(Error::Overloaded);
        }
        self.0.set(self.0.get() + bytes);
        Ok(MeterCharge(self.0.clone(), bytes))
    }
}

#[test]
fn constructor_does_not_open_kernel_resources() {
    let reactor = TestReactor::new(4, ());
    assert!(reactor.core().is_none());
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(reactor.poll_budgeted(8), Ok(0));
    reactor.wait(Duration::ZERO).unwrap();
    assert!(reactor.core().is_none());
    assert_eq!(
        TestReactor::new(0, ()).init(),
        Err(Error::InvalidConfiguration)
    );
}

#[test]
fn stopped_reactor_refuses_initialization_and_admission() {
    let reactor = TestReactor::new(4, ());
    reactor.shutdown(Duration::ZERO).unwrap();
    assert_eq!(reactor.init(), Err(Error::Unavailable));
    let reactor = TestReactor::new(4, ());
    drive(&reactor, reactor.drain()).unwrap();
    assert_eq!(
        poll(&mut reactor.readiness(null_fd(), libc::POLLIN as u32, &scope())),
        Poll::Ready(Err(Error::Unavailable))
    );
}

#[test]
fn operation_preserves_scope_and_buffer_errors_before_submission() {
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum AppError {
        Runtime(Error),
        Buffer,
    }
    impl From<Error> for AppError {
        fn from(error: Error) -> Self {
            Self::Runtime(error)
        }
    }
    impl From<uring::Error> for AppError {
        fn from(error: uring::Error) -> Self {
            Self::Runtime(error.into())
        }
    }
    #[derive(Clone)]
    struct AppScope(bool);
    impl LibraryScope for AppScope {
        type Error = AppError;
        fn check(&self) -> Result<(), AppError> {
            if self.0 {
                Err(AppError::Runtime(Error::DeadlineExceeded))
            } else {
                Ok(())
            }
        }
    }
    struct Rejecting;
    // SAFETY: never yields a region.
    unsafe impl IoBuffer for Rejecting {
        type Error = AppError;
        fn bytes(&self) -> Result<&[u8], AppError> {
            Err(AppError::Buffer)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8], AppError> {
            Err(AppError::Buffer)
        }
    }
    let reactor: Reactor<AppScope, ()> =
        Reactor::with_backend(4, (), RacerBackend::Scripted(Scripted::new(|_, _| Some(0))));
    let fd = null_fd();
    assert!(matches!(
        poll(&mut reactor.read_at(fd.clone(), 0, Rejecting, (), &AppScope(true))),
        Poll::Ready(Err(AppError::Runtime(Error::DeadlineExceeded)))
    ));
    assert!(matches!(
        poll(&mut reactor.read_at(fd.clone(), 0, Rejecting, (), &AppScope(false))),
        Poll::Ready(Err(AppError::Buffer))
    ));
    assert!(matches!(
        poll(&mut reactor.send(fd, Rejecting, (), &AppScope(false))),
        Poll::Ready(Err(AppError::Buffer))
    ));
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn rejected_budget_and_invalid_capacity_publish_no_kernel_owners() {
    let meter = Meter::default();
    meter.1.set(true);
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    let reactor = Reactor::<RequestScope, Meter>::with_backend(
        4,
        meter.clone(),
        RacerBackend::Scripted(Scripted::new(|_, _| Some(0))),
    )
    .with_rejection_observer(move |_, facts| seen.borrow_mut().push(facts.reason));
    assert_eq!(
        poll(&mut reactor.readiness(null_fd(), libc::POLLIN as u32, &scope())),
        Poll::Ready(Err(Error::Overloaded))
    );
    assert_eq!(*rejections.borrow(), [RejectionReason::Initialization]);
    assert!(reactor.core().is_none());
    meter.1.set(false);
    reactor.init().unwrap();
    assert!(meter.0.get() > 0, "ring memory is charged");
    let ring = meter.0.get();
    meter.1.set(true);
    assert_eq!(
        poll(&mut reactor.readiness(null_fd(), libc::POLLIN as u32, &scope())),
        Poll::Ready(Err(Error::Overloaded))
    );
    assert_eq!(rejections.borrow()[1], RejectionReason::Bookkeeping);
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(meter.0.get(), ring);
    assert!(reactor.reserve_submissions(5, ()).is_err());
    for interest in [0, libc::POLLPRI as u32] {
        assert_eq!(
            poll(&mut reactor.readiness(null_fd(), interest, &scope())),
            Poll::Ready(Err(Error::InvalidInput))
        );
    }
    drop(reactor);
    assert_eq!(meter.0.get(), 0, "ring charge released on drop");
}

#[test]
fn charges_and_leases_are_held_until_the_fence() {
    let meter = Meter::default();
    let backend = Scripted::new(|_, _| None);
    let state = backend.state();
    let reactor = Reactor::<RequestScope, Meter>::with_backend(
        4,
        meter.clone(),
        RacerBackend::Scripted(backend),
    );
    reactor.init().unwrap();
    let ring = meter.0.get();
    let lease = Rc::new(());
    let weak = Rc::downgrade(&lease);
    let fd = null_fd();
    let request = scope();
    let mut readiness =
        reactor.readiness_with_lease(fd.clone(), libc::POLLIN as u32, lease, &request);
    assert!(poll(&mut readiness).is_pending());
    assert_eq!(
        meter.0.get(),
        ring + TestReactor::bookkeeping::<Rc<()>, ()>()
    );
    drop(readiness);
    assert!(weak.upgrade().is_some(), "abandoned lease is retained");
    reactor.poll_budgeted(8).unwrap();
    reactor.poll_budgeted(8).unwrap();
    assert_eq!(reactor.in_flight(), 0);
    assert!(weak.upgrade().is_none());
    assert_eq!(meter.0.get(), ring);
    assert!(state.borrow().pending_kinds().is_empty());
}

#[test]
fn partitions_and_rejection_reports() {
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    let reactor = Reactor::<RequestScope, ()>::with_backend(
        3,
        (),
        RacerBackend::Scripted(Scripted::new(|_, _| None)),
    )
    .with_rejection_observer(move |scope, facts| seen.borrow_mut().push((scope.id, facts)));
    let pool = reactor.reserve_submissions(1, ()).unwrap();
    assert_eq!(
        reactor.reserve_submissions(1, ()).err(),
        Some(Error::InvalidConfiguration)
    );
    let fd = null_fd();
    let interest = libc::POLLIN as u32;
    let first = scope_id(1);
    let mut ordinary: Vec<_> = (0..2)
        .map(|_| reactor.readiness(fd.clone(), interest, &first))
        .collect();
    for op in &mut ordinary {
        assert!(poll(op).is_pending());
    }
    let full = scope_id(2);
    assert_eq!(
        poll(&mut reactor.readiness(fd.clone(), interest, &full)),
        Poll::Ready(Err(Error::Overloaded))
    );
    let mut reserved = reactor.readiness_reserved(fd.clone(), interest, (), pool.clone(), &first);
    assert!(poll(&mut reserved).is_pending());
    assert_eq!(pool.active(), 1);
    assert_eq!(
        poll(&mut reactor.readiness_reserved(fd.clone(), interest, (), pool.clone(), &full)),
        Poll::Ready(Err(Error::Overloaded))
    );
    let reports = rejections.borrow().clone();
    assert_eq!(reports.len(), 2);
    assert_eq!(reports[0].0, 2);
    assert_eq!(reports[0].1.reason, RejectionReason::OrdinaryFull);
    assert_eq!(reports[0].1.kind, SubmissionKind::Readiness);
    assert_eq!((reports[0].1.used, reports[0].1.limit), (Some(2), Some(2)));
    assert_eq!(reports[1].1.reason, RejectionReason::ReservedFull);
    assert!(reports[1].1.reserved);
    // A foreign partition is a configuration error.
    let other = TestReactor::new(2, ()).reserve_submissions(1, ()).unwrap();
    assert_eq!(
        poll(&mut reactor.readiness_reserved(fd.clone(), interest, (), other, &full)),
        Poll::Ready(Err(Error::InvalidConfiguration))
    );
    drop((ordinary, reserved));
    reactor.shutdown(Duration::from_secs(1)).unwrap();
    assert_eq!(pool.active(), 0);
}

#[test]
fn queue_full_is_reported_as_submission_queue_full() {
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    let backend = Scripted::new(|_, _| None);
    let state = backend.state();
    let reactor = Reactor::<RequestScope, ()>::with_backend(2, (), RacerBackend::Scripted(backend))
        .with_rejection_observer(move |_, facts| seen.borrow_mut().push(facts));
    reactor.init().unwrap();
    state.borrow_mut().full = 2;
    assert_eq!(
        poll(&mut reactor.readiness(null_fd(), libc::POLLIN as u32, &scope())),
        Poll::Ready(Err(Error::Overloaded))
    );
    let reports = rejections.borrow();
    assert_eq!(reports.len(), 1);
    assert_eq!(reports[0].reason, RejectionReason::SubmissionQueueFull);
    assert_eq!(reports[0].kind, SubmissionKind::Readiness);
    assert_eq!(reactor.in_flight(), 0);
}

/// A scripted sequence of data results; readiness polls report POLLIN.
fn sequence(results: Vec<i32>) -> impl FnMut(OpKind, scripted::Region) -> Option<i32> {
    let mut results = results.into_iter();
    move |kind, _| match kind {
        OpKind::PollAdd => Some(libc::POLLIN as i32),
        _ => results.next(),
    }
}

#[test]
fn transient_errors_retry_with_readiness_and_return_partial_progress_once() {
    let (reactor, state) = scripted(4, sequence(vec![-libc::EINTR, -libc::EAGAIN, 3]));
    let lease = Rc::new(());
    let completion = drive(
        &reactor,
        reactor.recv(null_fd(), Bytes(vec![0; 8]), lease.clone(), &scope()),
    )
    .unwrap();
    assert_eq!(completion.bytes, 3);
    assert_eq!(completion.buffer.0.len(), 8);
    assert!(Rc::ptr_eq(&completion.lease, &lease));
    let kinds: Vec<_> = state
        .borrow()
        .pushes
        .iter()
        .map(|(kind, _)| *kind)
        .collect();
    assert_eq!(
        kinds,
        [OpKind::Recv, OpKind::Recv, OpKind::PollAdd, OpKind::Recv]
    );
}

#[test]
fn retries_are_bounded_and_keep_the_last_errno() {
    for errno in [libc::EINTR, libc::EAGAIN] {
        let (reactor, state) = scripted(4, sequence(vec![-errno; 20]));
        assert_eq!(
            drive(
                &reactor,
                reactor.write_at(null_fd(), 0, Bytes(vec![1; 4]), (), &scope())
            )
            .err(),
            Some(Error::Os(errno))
        );
        let writes = state
            .borrow()
            .pushes
            .iter()
            .filter(|(kind, _)| *kind == OpKind::Write)
            .count();
        assert_eq!(writes, DATA_RETRIES + 1);
    }
    let (reactor, state) = scripted(4, sequence(vec![-libc::EPIPE]));
    assert_eq!(
        drive(
            &reactor,
            reactor.send(null_fd(), Bytes(vec![1]), (), &scope())
        )
        .err(),
        Some(Error::Os(libc::EPIPE))
    );
    assert_eq!(
        state.borrow().pushes.len(),
        1,
        "other errors are not retried"
    );
}

#[test]
fn cancellation_between_attempts_prevents_resubmission() {
    let request = scope();
    let canceler = request.clone();
    let (reactor, state) = scripted(4, move |kind, _| match kind {
        OpKind::Read => {
            canceler.cancel();
            Some(-libc::EINTR)
        }
        _ => Some(0),
    });
    assert_eq!(
        drive(
            &reactor,
            reactor.read_at(null_fd(), 0, Bytes(vec![0; 4]), (), &request)
        )
        .err(),
        Some(Error::Canceled)
    );
    assert_eq!(state.borrow().pushes.len(), 1);
}

#[test]
fn sentinel_offsets_are_rejected_before_publication() {
    let (reactor, state) = scripted(4, |_, _| Some(0));
    for offset in [u64::MAX, i64::MAX as u64 + 1] {
        assert!(matches!(
            poll(&mut reactor.read_at(null_fd(), offset, Bytes(vec![0]), (), &scope())),
            Poll::Ready(Err(Error::InvalidInput))
        ));
        assert!(matches!(
            poll(&mut reactor.write_at(null_fd(), offset, Bytes(vec![0]), (), &scope())),
            Poll::Ready(Err(Error::InvalidInput))
        ));
    }
    assert!(state.borrow().pushes.is_empty());
}

#[test]
fn offloaded_file_io_sets_the_hint_and_sockets_do_not() {
    let (reactor, state) = scripted(4, |_, _| Some(1));
    let fd = null_fd();
    drive(
        &reactor,
        reactor.read_at_offloaded(fd.clone(), 0, Bytes(vec![0]), (), &scope()),
    )
    .unwrap();
    drive(
        &reactor,
        reactor.write_at_offloaded(fd.clone(), 0, Bytes(vec![0]), (), &scope()),
    )
    .unwrap();
    drive(
        &reactor,
        reactor.read_at(fd.clone(), 0, Bytes(vec![0]), (), &scope()),
    )
    .unwrap();
    drive(&reactor, reactor.send(fd, Bytes(vec![0]), (), &scope())).unwrap();
    let offload = PushHints::new().with_offload(true);
    assert_eq!(
        state.borrow().pushes,
        [
            (OpKind::Read, offload),
            (OpKind::Write, offload),
            (OpKind::Read, PushHints::new()),
            (OpKind::Send, PushHints::new()),
        ]
    );
}

/// Immutable body storage shared with other owners.
struct SharedBody(Rc<Vec<u8>>);

// SAFETY: no owner mutates or resizes the shared Vec, so its bytes are stable.
unsafe impl SendBuffer for SharedBody {
    type Error = Error;

    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }
}

#[test]
fn vectored_write_retries_offloads_and_returns_both_owners() {
    let (reactor, state) = scripted(4, sequence(vec![-libc::EINTR, -libc::EAGAIN, 5, 2]));
    let body = Rc::new(b"body".to_vec());
    let lease = Rc::new(());
    let completion = drive(
        &reactor,
        reactor.write_vectored_at(
            null_fd(),
            8,
            Bytes(b"HDR".to_vec()),
            SharedBody(body.clone()),
            lease.clone(),
            &scope(),
        ),
    )
    .unwrap();
    assert_eq!(completion.bytes, 5);
    assert_eq!(completion.header.0, b"HDR");
    assert!(Rc::ptr_eq(&completion.body.0, &body));
    assert!(Rc::ptr_eq(&completion.lease, &lease));
    let offloaded = drive(
        &reactor,
        reactor.write_vectored_at_offloaded(
            null_fd(),
            0,
            Bytes(vec![1]),
            Bytes(vec![2]),
            (),
            &scope(),
        ),
    )
    .unwrap();
    assert_eq!(offloaded.bytes, 2);
    let offload = PushHints::new().with_offload(true);
    assert_eq!(
        state.borrow().pushes,
        [
            (OpKind::Writev, PushHints::new()),
            (OpKind::Writev, PushHints::new()),
            (OpKind::PollAdd, PushHints::new()),
            (OpKind::Writev, PushHints::new()),
            (OpKind::Writev, offload),
        ]
    );
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn vectored_write_rejects_invalid_input_and_reports_errors() {
    struct RejectingBody;
    // SAFETY: never yields a region.
    unsafe impl SendBuffer for RejectingBody {
        type Error = Error;
        fn send_bytes(&self) -> Result<&[u8]> {
            Err(Error::InvalidInput)
        }
    }
    let (reactor, state) = scripted(4, sequence(vec![-libc::EIO]));
    for offset in [u64::MAX, i64::MAX as u64 + 1] {
        assert!(matches!(
            poll(&mut reactor.write_vectored_at(
                null_fd(),
                offset,
                Bytes(vec![0]),
                Bytes(vec![0]),
                (),
                &scope()
            )),
            Poll::Ready(Err(Error::InvalidInput))
        ));
    }
    assert!(matches!(
        poll(&mut reactor.write_vectored_at(
            null_fd(),
            0,
            Bytes(vec![0]),
            RejectingBody,
            (),
            &scope()
        )),
        Poll::Ready(Err(Error::InvalidInput))
    ));
    assert!(state.borrow().pushes.is_empty());
    assert_eq!(
        drive(
            &reactor,
            reactor.write_vectored_at(null_fd(), 0, Bytes(vec![0]), Bytes(vec![1]), (), &scope())
        )
        .err(),
        Some(Error::Os(libc::EIO))
    );
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn fence_matching_cancels_only_matching_scopes() {
    let (reactor, state) = scripted(4, |_, _| None);
    let fd = null_fd();
    let (one, two) = (scope_id(1), scope_id(2));
    let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &one);
    let mut second = reactor.readiness(fd.clone(), libc::POLLIN as u32, &two);
    assert!(poll(&mut first).is_pending());
    assert!(poll(&mut second).is_pending());
    drive(&reactor, reactor.fence_matching(|scope| scope.id == 1)).unwrap();
    assert_eq!(poll(&mut first), Poll::Ready(Err(Error::Canceled)));
    assert!(poll(&mut second).is_pending());
    state
        .borrow_mut()
        .complete(OpKind::PollAdd, libc::POLLIN as i32);
    assert_eq!(
        drive(&reactor, second),
        Ok(libc::POLLIN as u32),
        "kernel results win"
    );
}

#[test]
fn successful_completion_wins_over_late_cancellation() {
    let (reactor, state) = scripted(4, |_, _| None);
    let request = scope();
    let mut op = reactor.readiness(null_fd(), libc::POLLIN as u32, &request);
    assert!(poll(&mut op).is_pending());
    reactor.poll_budgeted(8).unwrap();
    state
        .borrow_mut()
        .complete(OpKind::PollAdd, libc::POLLOUT as i32);
    request.cancel();
    reactor.poll_budgeted(8).unwrap();
    assert_eq!(poll(&mut op), Poll::Ready(Ok(libc::POLLOUT as u32)));
}

#[test]
fn rejection_reasons_map_library_kinds() {
    assert_eq!(submission_kind(OpKind::PollAdd), SubmissionKind::Readiness);
    assert_eq!(submission_kind(OpKind::Timeout), SubmissionKind::Timeout);
    assert_eq!(submission_kind(OpKind::Read), SubmissionKind::OtherIo);
}

#[test]
fn connect_rejects_unnamed_unix_paths_and_observes_errno() {
    let (reactor, _state) = scripted(4, |kind, _| {
        (kind == OpKind::Connect).then_some(-libc::ECONNREFUSED)
    });
    assert_eq!(
        poll(&mut reactor.connect(
            null_fd(),
            SocketAddress::from(std::path::PathBuf::new()),
            &scope()
        )),
        Poll::Ready(Err(Error::InvalidInput))
    );
    let errno = Rc::new(Cell::new(None));
    let lease = Rc::new(());
    assert_eq!(
        drive(
            &reactor,
            reactor.connect_with_observation(
                null_fd(),
                SocketAddress::from("127.0.0.1:1".parse::<std::net::SocketAddr>().unwrap()),
                lease.clone(),
                Some(errno.clone()),
                &scope(),
            )
        )
        .err(),
        Some(Error::Os(libc::ECONNREFUSED))
    );
    assert_eq!(errno.get(), Some(libc::ECONNREFUSED));
    assert_eq!(Rc::strong_count(&lease), 1);
}

mod kernel {
    //! Real io_uring behavior through the adapter.
    use super::*;
    use std::io::{Read, Write};

    #[test]
    fn offset_file_io_and_charge_release() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let file = tempfile();
        let written = drive(
            &reactor,
            reactor.write_at(file.clone(), 3, Bytes(b"hello".to_vec()), 7u8, &scope()),
        )
        .unwrap();
        assert_eq!((written.bytes, written.lease), (5, 7));
        let read = drive(
            &reactor,
            reactor.read_at(file.clone(), 0, Bytes(vec![9; 16]), (), &scope()),
        )
        .unwrap();
        assert_eq!(read.bytes, 8);
        assert_eq!(&read.buffer.0[..8], b"\0\0\0hello");
        let read = drive(
            &reactor,
            reactor.read_at_offloaded(file, 3, Bytes(vec![0; 5]), (), &scope()),
        )
        .unwrap();
        assert_eq!(&read.buffer.0, b"hello");
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn vectored_file_write() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let file = tempfile();
        let body = Rc::new(b"world".to_vec());
        let written = drive(
            &reactor,
            reactor.write_vectored_at(
                file.clone(),
                2,
                Bytes(b"hello ".to_vec()),
                SharedBody(body.clone()),
                7u8,
                &scope(),
            ),
        )
        .unwrap();
        assert_eq!((written.bytes, written.lease), (11, 7));
        assert!(Rc::ptr_eq(&written.body.0, &body));
        let written = drive(
            &reactor,
            reactor.write_vectored_at_offloaded(
                file.clone(),
                0,
                Bytes(b"<".to_vec()),
                Bytes(b">".to_vec()),
                (),
                &scope(),
            ),
        )
        .unwrap();
        assert_eq!(written.bytes, 2);
        let read = drive(
            &reactor,
            reactor.read_at(file, 0, Bytes(vec![9; 16]), (), &scope()),
        )
        .unwrap();
        assert_eq!(read.bytes, 13);
        assert_eq!(&read.buffer.0[..13], b"<>hello world");
        let error = drive(
            &reactor,
            reactor.write_vectored_at(null_fd(), 0, Bytes(vec![1]), Bytes(vec![2]), (), &scope()),
        )
        .err();
        assert_eq!(error, Some(Error::Os(libc::EBADF)));
        assert_eq!(reactor.in_flight(), 0);
    }

    fn tempfile() -> Rc<Descriptor> {
        // SAFETY: memfd_create returns a fresh descriptor or -1.
        let fd = unsafe { libc::memfd_create(c"adapter".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: the descriptor is fresh and owned by nobody else.
        Rc::new(unsafe { <Descriptor as std::os::fd::FromRawFd>::from_raw_fd(fd) })
    }

    #[test]
    fn socket_short_io_readiness_eof_and_broken_pipe() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let (left, mut right) = std::os::unix::net::UnixStream::pair().unwrap();
        left.set_nonblocking(true).unwrap();
        let left = Rc::new(Descriptor::from(left));
        right.write_all(b"abc").unwrap();
        let ready = drive(
            &reactor,
            reactor.readiness(left.clone(), libc::POLLIN as u32, &scope()),
        )
        .unwrap();
        assert_ne!(ready & libc::POLLIN as u32, 0);
        let received = drive(
            &reactor,
            reactor.recv(left.clone(), Bytes(vec![0; 8]), (), &scope()),
        )
        .unwrap();
        assert_eq!(&received.buffer.0[..received.bytes], b"abc");
        // EAGAIN is retried after readiness: the receive waits for data.
        let request = scope();
        let mut pending = reactor.recv(left.clone(), Bytes(vec![0; 8]), (), &request);
        for _ in 0..3 {
            assert!(poll(&mut pending).is_pending());
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        right.write_all(b"z").unwrap();
        assert_eq!(drive(&reactor, pending).unwrap().bytes, 1);
        let sent = drive(
            &reactor,
            reactor.send(left.clone(), Bytes(b"pong".to_vec()), (), &scope()),
        )
        .unwrap();
        assert_eq!(sent.bytes, 4);
        let mut buffer = [0; 4];
        right.read_exact(&mut buffer).unwrap();
        assert_eq!(&buffer, b"pong");
        drop(right);
        let eof = drive(
            &reactor,
            reactor.recv(left.clone(), Bytes(vec![0; 8]), (), &scope()),
        )
        .unwrap();
        assert_eq!(eof.bytes, 0);
        assert_eq!(
            drive(
                &reactor,
                reactor.send(left, Bytes(b"x".to_vec()), (), &scope())
            )
            .err(),
            Some(Error::Os(libc::EPIPE))
        );
    }

    #[test]
    fn tcp_accept_connect_and_refused_errno() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let listener = Descriptor::tcp_listener("127.0.0.1:0".parse().unwrap()).unwrap();
        let address = listener.tcp_tuple().map(|(local, _)| local);
        assert!(address.is_none(), "a listener has no peer");
        let owned = listener.into_host().unwrap();
        let std = std::net::TcpListener::from(owned);
        let address = std.local_addr().unwrap();
        let listener = Rc::new(Descriptor::from(std));
        let client = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
        let request = scope();
        let mut accept = reactor.accept(listener.clone(), &request);
        assert!(poll(&mut accept).is_pending());
        let lease = drive(
            &reactor,
            reactor.connect_with_lease(client.clone(), SocketAddress::from(address), 5u8, &request),
        )
        .unwrap();
        assert_eq!(lease, 5);
        let accepted = drive(&reactor, accept).unwrap();
        let (local, peer) = accepted.tcp_tuple().unwrap();
        assert_eq!(local, address);
        assert_eq!(Some(peer), client.tcp_tuple().map(|(local, _)| local));
        // Connection refused keeps its errno.
        drop(listener);
        let refused = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
        let errno = Rc::new(Cell::new(None));
        let result = drive(
            &reactor,
            reactor.connect_with_observation(
                refused,
                SocketAddress::from(address),
                (),
                Some(errno.clone()),
                &request,
            ),
        );
        assert_eq!(result.err(), Some(Error::Os(libc::ECONNREFUSED)));
        assert_eq!(errno.get(), Some(libc::ECONNREFUSED));
    }

    #[test]
    fn unix_connect_keeps_sockaddr_alive() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let dir = std::env::temp_dir().join(format!("racer-platform-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("socket");
        let _ = std::fs::remove_file(&path);
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let client = Rc::new(Descriptor::socket(libc::AF_UNIX).unwrap());
        drive(
            &reactor,
            reactor.connect(client, SocketAddress::from(path.clone()), &scope()),
        )
        .unwrap();
        listener.accept().unwrap();
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn cancellation_abandonment_and_drop_fence() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let (left, _right) = std::os::unix::net::UnixStream::pair().unwrap();
        let left = Rc::new(Descriptor::from(left));
        let request = scope();
        let lease = Rc::new(());
        let weak = Rc::downgrade(&lease);
        let mut op =
            reactor.readiness_with_lease(left.clone(), libc::POLLIN as u32, lease, &request);
        assert!(poll(&mut op).is_pending());
        reactor.poll_budgeted(8).unwrap();
        assert_eq!(reactor.in_flight(), 1);
        request.cancel();
        assert_eq!(drive(&reactor, op), Err(Error::Canceled));
        assert!(weak.upgrade().is_none());
        assert_eq!(reactor.in_flight(), 0);
        // An abandoned operation keeps its descriptor until fenced.
        let lease = Rc::new(());
        let weak = Rc::downgrade(&lease);
        let other = scope();
        let mut op = reactor.readiness_with_lease(left, libc::POLLIN as u32, lease, &other);
        assert!(poll(&mut op).is_pending());
        drop(op);
        assert!(weak.upgrade().is_some());
        drive(&reactor, reactor.drain()).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(
            poll(&mut reactor.readiness(null_fd(), libc::POLLIN as u32, &scope())),
            Poll::Ready(Err(Error::Unavailable))
        );
    }

    #[test]
    fn drop_fences_outstanding_work() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let (left, _right) = std::os::unix::net::UnixStream::pair().unwrap();
        let lease = Rc::new(());
        let weak = Rc::downgrade(&lease);
        let request = scope();
        let mut op = reactor.readiness_with_lease(
            Rc::new(Descriptor::from(left)),
            libc::POLLIN as u32,
            lease,
            &request,
        );
        assert!(poll(&mut op).is_pending());
        drop(op);
        drop(reactor);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn kernel_timer_completes_and_cancel_fences() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let request = scope();
        drive(
            &reactor,
            reactor.sleep_until(
                crate::environment::now(),
                timer::SleepMode::Kernel,
                &request,
            ),
        )
        .unwrap();
        let until = crate::environment::now() + Duration::from_millis(2);
        drive(
            &reactor,
            reactor.sleep_until(until, timer::SleepMode::Kernel, &request),
        )
        .unwrap();
        assert!(crate::environment::now() >= until);
        let mut future = reactor.sleep_until(
            crate::environment::now() + Duration::from_secs(10),
            timer::SleepMode::Kernel,
            &request,
        );
        assert!(poll(&mut future).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        request.cancel();
        assert_eq!(drive(&reactor, future), Err(Error::Canceled));
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn external_wake_interrupts_wait() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let wake = reactor.waker().unwrap();
        let remote = wake.clone();
        std::thread::spawn(move || remote.wake().unwrap())
            .join()
            .unwrap();
        let started = Instant::now();
        reactor.wait(Duration::from_secs(5)).unwrap();
        assert!(started.elapsed() < Duration::from_secs(1));
        wake.wake().unwrap();
        let started = Instant::now();
        reactor.wait(Duration::from_secs(5)).unwrap();
        assert!(started.elapsed() < Duration::from_secs(1));
        // An uninitialized reactor initializes on waker().
        let fresh = TestReactor::new(2, ());
        fresh.waker().unwrap().wake().unwrap();
        assert!(fresh.core().is_some());
    }
}
