//! Adapter contracts: scope precedence, scope scans, scope fences, partitions,
//! and rejection observation. Ring ownership is covered by `racer-io`.
use super::*;
use fixtures::{Limits, Reactor, RequestScope, host_env, reactor};
use std::{num::NonZeroUsize, task::Context, time::Instant};

pub(super) mod fixtures {
    //! A scoped adapter with an explicit backend for raw completion ownership tests.
    use super::*;
    use crate::{CancelToken, Env, WorkerCtx};

    /// Queue ceiling selected by each ownership regression.
    pub struct Limits {
        /// Maximum accepted operations across ordinary and reserved partitions.
        pub queue_entries: NonZeroUsize,
    }

    /// The adapter under test.
    pub type Reactor = super::super::Reactor<RequestScope>;

    /// Build a lazy reactor on `env` with the selected queue ceiling.
    pub fn reactor(limits: Limits, env: &Env) -> Reactor {
        Reactor::new(limits.queue_entries.get(), env)
    }

    /// A kernel backend on host time.
    pub fn host_env() -> Env {
        Env::new(crate::Driver::Uring, WorkerCtx::host())
    }

    /// Deadline and cancellation policy retained independently by each operation.
    #[derive(Clone)]
    pub struct RequestScope {
        /// Absolute host deadline.
        pub deadline: Instant,

        /// Shared cancellation state, checked before deadline expiration.
        pub cancellation: CancelToken,
    }

    impl RequestScope {
        /// Create a fresh cancellation source with a caller-selected deadline.
        pub fn new(deadline: Instant) -> Self {
            Self {
                deadline,
                cancellation: CancelToken::new(),
            }
        }

        /// Request cancellation without treating the request as a kernel fence.
        pub fn cancel(&self) {
            self.cancellation.cancel();
        }
    }

    impl Scope for RequestScope {
        /// Runtime classification used by ownership regressions.
        type Error = Error;

        /// Give cancellation precedence over expiration on host time.
        fn check(&self) -> Result<()> {
            if self.cancellation.is_canceled() {
                Err(Error::Canceled)
            } else if Instant::now() >= self.deadline {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }
    }
}

/// Build a nonzero queue ceiling for a regression's admitted operation count.
fn limits(capacity: usize) -> Limits {
    Limits {
        queue_entries: NonZeroUsize::new(capacity).unwrap(),
    }
}

/// Create fresh cancellation state and a five-second deadline for a test operation.
pub(super) fn scope() -> RequestScope {
    RequestScope::new(Instant::now() + Duration::from_secs(5))
}

/// Poll delivery once without advancing the kernel or simulated driver.
pub(super) fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}

/// Drive explicit completion turns under a host deadline until delivery finishes.
pub(super) fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

/// Initialize a host reactor, skipping only unsupported or policy-denied io_uring.
pub(super) fn kernel_reactor(capacity: usize) -> Option<Reactor> {
    // Skip only when the kernel lacks io_uring or policy denies ring creation.
    // Configuration, quota, opcode, and ordinary I/O errors must fail tests.
    match io_uring::IoUring::new(2) {
        Ok(ring) => drop(ring),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
            ) =>
        {
            assert_ne!(
                std::env::var("RUNTIME_REQUIRE_IO_URING").as_deref(),
                Ok("1"),
                "RUNTIME_REQUIRE_IO_URING=1 but io_uring is unavailable: {error}"
            );
            eprintln!("io_uring kernel test unavailable: {error}");
            return None;
        }
        Err(error) => panic!("unexpected io_uring setup failure: {error}"),
    }
    let reactor = reactor(limits(capacity), &host_env());
    reactor.init().unwrap();
    Some(reactor)
}

mod adapter {
    use super::*;
    use crate::{Env, WorkerCtx, sim::SimOs};

    fn sim_reactor(capacity: usize) -> (SimOs, (), Reactor) {
        let sim = SimOs::new();
        let reactor = reactor(limits(capacity), &Env::sim(&sim, WorkerCtx::host()));
        (sim, (), reactor)
    }

    #[test]
    fn failed_scope_is_reported_before_submission() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_left, right) = sim.socket_pair();
        let scope = scope();
        scope.cancel();
        let mut recv = r.recv(Rc::new(right), vec![0; 1], (), &scope);
        assert!(matches!(poll(&mut recv), Poll::Ready(Err(Error::Canceled))));
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn scan_cancels_operations_whose_scope_fails_and_scope_error_wins() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_left, right) = sim.socket_pair();
        let clock = Instant::now();
        let scope = RequestScope::new(clock + Duration::from_secs(60));
        let mut recv = r.recv(Rc::new(right), vec![0; 1], (), &scope);
        assert!(poll(&mut recv).is_pending());
        assert_eq!(r.in_flight(), 1);
        // A deadline-only scope fails its check without a cancel token firing:
        // the scan must cancel the kernel operation.
        let mut expired = scope.clone();
        expired.deadline = clock;
        let mut recv_expired = {
            let (_l, rr) = sim.socket_pair();
            r.recv(Rc::new(rr), vec![0; 1], (), &expired)
        };
        assert!(matches!(
            poll(&mut recv_expired),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        scope.cancel();
        assert!(matches!(drive(&r, recv), Err(Error::Canceled)));
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn budgeted_scan_rotates_past_long_lived_operations() {
        let (sim, _environment, r) = sim_reactor(16);
        let mut sockets = Vec::new();
        let idle = scope();
        let mut idle_recvs: Vec<_> = (0..10)
            .map(|_| {
                let (left, right) = sim.socket_pair();
                sockets.push(left);
                r.recv(Rc::new(right), vec![0; 1], (), &idle)
            })
            .collect();
        for recv in &mut idle_recvs {
            assert!(poll(recv).is_pending());
        }
        let (_left, right) = sim.socket_pair();
        let late = scope();
        let mut recv = r.recv(Rc::new(right), vec![0; 1], (), &late);
        assert!(poll(&mut recv).is_pending());
        late.cancel();
        // Budget 4 over 11 records: a busy lane reaches the newest record
        // within a bounded number of turns instead of rechecking the oldest.
        let mut result = None;
        for _ in 0..8 {
            r.poll_budgeted(4).unwrap();
            if let Poll::Ready(done) = poll(&mut recv) {
                result = Some(done);
                break;
            }
        }
        assert!(matches!(result, Some(Err(Error::Canceled))));
        for recv in &mut idle_recvs {
            assert!(poll(recv).is_pending());
        }
    }

    #[test]
    fn fence_matching_cancels_only_matching_scopes() {
        let (sim, _environment, r) = sim_reactor(4);
        let (_a, left) = sim.socket_pair();
        let (_b, right) = sim.socket_pair();
        let first = scope();
        let second = scope();
        let mut one = r.recv(Rc::new(left), vec![0; 1], (), &first);
        let mut two = r.recv(Rc::new(right), vec![0; 1], (), &second);
        assert!(poll(&mut one).is_pending());
        assert!(poll(&mut two).is_pending());
        let target = first.cancellation.clone();
        drive(
            &r,
            r.fence_matching(move |scope| scope.cancellation.same(&target)),
        )
        .unwrap();
        assert!(matches!(poll(&mut one), Poll::Ready(Err(Error::Canceled))));
        assert!(poll(&mut two).is_pending());
        assert_eq!(r.in_flight(), 1);
        drop(two);
        drive(&r, r.drain()).unwrap();
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn reserved_partition_keeps_control_slots_and_reports_rejections() {
        let sim = SimOs::new();
        let rejections = Rc::new(RefCell::new(Vec::new()));
        let seen = rejections.clone();
        let r = Reactor::new(2, &Env::sim(&sim, WorkerCtx::host())).with_rejection_observer(
            move |_, facts: SubmissionRejection| seen.borrow_mut().push(facts.reason),
        );
        let capacity = r.reserve_submissions(1, ()).unwrap();
        assert_eq!(capacity.capacity(), 1);
        assert!(r.reserve_submissions(1, ()).is_err());
        let scope = scope();
        let (_a, a) = sim.socket_pair();
        let (_b, b) = sim.socket_pair();
        let (_c, c) = sim.socket_pair();
        let mut ordinary = r.recv(Rc::new(a), vec![0; 1], (), &scope);
        let mut rejected = r.recv(Rc::new(b), vec![0; 1], (), &scope);
        let mut control = r.recv_reserved(Rc::new(c), vec![0; 1], capacity.clone(), &scope);
        assert!(poll(&mut ordinary).is_pending());
        assert!(matches!(
            poll(&mut rejected),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(poll(&mut control).is_pending());
        assert!(matches!(
            rejections.borrow().as_slice(),
            [RejectionReason::OrdinaryFull]
        ));
        drop((ordinary, control));
        r.shutdown(Duration::from_secs(5)).unwrap();
    }

    #[test]
    fn connect_observation_records_errno() {
        let (sim, _environment, r) = sim_reactor(4);
        let socket = Rc::new(sim.socket(libc::AF_INET).unwrap());
        let observed = Rc::new(Cell::new(None));
        let address = SocketAddress::Inet("127.0.0.1:9".parse().unwrap());
        let result = drive(
            &r,
            r.connect_with_observation(socket, address, (), Some(observed.clone()), &scope()),
        );
        assert!(matches!(result, Err(Error::Os(_))));
        assert!(observed.get().is_some());
    }
}

#[test]
fn constructor_does_not_open_kernel_resources() {
    let r = reactor(limits(1), &host_env());
    assert_eq!(r.in_flight(), 0);
    assert_eq!(r.poll_budgeted(8), Ok(0));
}

#[test]
fn kernel_reactor_reads_and_writes_files() {
    let Some(r) = kernel_reactor(4) else {
        return;
    };
    let path = std::env::temp_dir().join(format!("racer-io-scoped-{}", std::process::id()));
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .read(true)
        .write(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(&path).unwrap();
    let fd = Rc::new(Descriptor::from(std::os::fd::OwnedFd::from(file)));
    let scope = scope();
    let written = drive(&r, r.write_at(fd.clone(), 3, b"abc".to_vec(), (), &scope)).unwrap();
    assert_eq!(written.bytes, 3);
    let read = drive(&r, r.read_at(fd, 0, vec![9; 6], (), &scope)).unwrap();
    assert_eq!(read.buffer, [0, 0, 0, b'a', b'b', b'c']);
}

mod retry {
    //! Overload pacing on the reactor's timer heap.
    use super::super::retry::RetryOptions;
    use super::*;
    use crate::{Env, SimClock, sim::SimOs, xthread::tests::WakeCounter};
    use std::{cell::Cell, sync::Arc, task::Waker};

    /// Scope whose failure the fixture selects.
    #[derive(Clone, Default)]
    struct TestScope(Rc<Cell<Option<Error>>>);

    impl Scope for TestScope {
        type Error = Error;

        fn check(&self) -> Result<()> {
            self.0.get().map_or(Ok(()), Err)
        }
    }

    /// A simulated reactor on a virtual clock.
    fn sim(seed: u64) -> (SimClock, super::super::Reactor<TestScope>) {
        let clock = SimClock::new(seed);
        let reactor = super::super::Reactor::new(8, &Env::sim(&SimOs::new(), clock.ctx(0)));
        (clock, reactor)
    }

    /// Retry bounds and invalid intervals fail without unbounded or immediate resubmission.
    #[test]
    fn configured_retry_limit_interval_zero_and_overflow() {
        let (clock, r) = sim(4);
        let scope = TestScope::default();
        let attempts = Cell::new(0);
        let mut future = r.retry_with(
            &scope,
            RetryOptions {
                interval: Duration::from_millis(3),
                max_retries: Some(1),
            },
            || {
                attempts.set(attempts.get() + 1);
                Box::pin(async { Err::<(), _>(Error::Overloaded) })
            },
        );
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        clock.advance(Duration::from_millis(2));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(attempts.get(), 1);
        clock.advance(Duration::from_millis(1));
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        );
        assert_eq!(attempts.get(), 2);
        for (interval, error) in [
            (Duration::ZERO, Error::InvalidConfiguration),
            (Duration::MAX, Error::InvalidInput),
        ] {
            let mut future = r.retry_with(
                &scope,
                RetryOptions {
                    interval,
                    max_retries: None,
                },
                || Box::pin(async { Err::<(), _>(Error::Overloaded) }),
            );
            assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
        }
    }

    /// Default pacing waits 10ms on a reactor timer, without self-wakes, and
    /// rechecks the scope before resubmission.
    #[test]
    fn retries_only_after_ten_ms_without_self_wakes_and_checks_scope() {
        for cancel in [false, true] {
            let (clock, r) = sim(45);
            let scope = TestScope::default();
            let attempts = Cell::new(0);
            let wake = Arc::new(WakeCounter::default());
            let waker = Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            let mut future = r.retry_overloaded(&scope, || {
                let attempt = attempts.get();
                attempts.set(attempt + 1);
                Box::pin(async move {
                    if attempt == 0 {
                        Err(Error::Overloaded)
                    } else {
                        Ok(7)
                    }
                })
            });
            assert!(future.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(9));
            r.poll_budgeted(8).unwrap();
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(attempts.get(), 1);
            assert_eq!(wake.count(), 0);
            if cancel {
                scope.0.set(Some(Error::Canceled));
            }
            clock.advance(Duration::from_millis(1));
            r.poll_budgeted(8).unwrap();
            assert_eq!(wake.count(), 1, "the backoff timer wakes the task once");
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(if cancel { Err(Error::Canceled) } else { Ok(7) })
            );
            assert_eq!(attempts.get(), if cancel { 1 } else { 2 });
        }
    }

    /// Scope failure cannot truncate an accepted operation's completion fence.
    #[test]
    fn accepted_pending_operation_is_not_truncated_by_scope_failure() {
        let (_clock, r) = sim(47);
        let scope = TestScope::default();
        let completed = Cell::new(false);
        let attempts = Cell::new(0);
        let mut future = r.retry_overloaded(&scope, || {
            attempts.set(attempts.get() + 1);
            Box::pin(std::future::poll_fn(|_| {
                if completed.get() {
                    Poll::Ready(Err::<(), _>(Error::Io))
                } else {
                    Poll::Pending
                }
            }))
        });
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.0.set(Some(Error::Canceled));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        completed.set(true);
        assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(Error::Io)));
        assert_eq!(attempts.get(), 1);
    }
}
